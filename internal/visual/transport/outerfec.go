package transport

import (
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/reedsolomon"
)

// Outer FEC groups N application payloads and appends M RS parity payloads.
// The parity payloads are transmitted as ordinary inner-transport messages so
// the inner codec and framing layer remain completely unaware of them.
//
// Wire format of every outer-FEC-wrapped payload:
//
//	offset  size  field
//	0       1     magic  (0xBB)
//	1       4     groupID (uint32 big-endian)
//	5       1     N      (data shards per group)
//	6       1     M      (parity shards per group)
//	7       1     index  (0..N-1 = data shard, N..N+M-1 = parity shard)
//	8       ...   payload (original data bytes or RS parity shard)
//
// Data payloads carry the original application bytes at their natural size.
// Parity payloads carry an opaque RS shard of exactly shardSize bytes, where
// shardSize = maxLen(group)+2 (the +2 is a uint16 length prefix so the decoder
// can strip padding from recovered data shards).
const outerHeaderSize = 8
const outerMagicByte uint8 = 0xBB

// ─── Encoder ──────────────────────────────────────────────────────────────────

// OuterFECEncoder wraps application payloads with an outer-group header and,
// after every N payloads, appends M RS parity payloads.
//
// The encoder is NOT safe for concurrent use; it is intended to be driven by
// a single goroutine (the visual frame pump).
type OuterFECEncoder struct {
	n, m         int
	buf          [][]byte // data payloads accumulated for current group
	maxLen       int      // max payload length seen in the current group
	currentGroup atomic.Uint32
}

// NewOuterFECEncoder creates an encoder with n data payloads and m parity
// payloads per group.  Plan default is n=10, m=4.
func NewOuterFECEncoder(n, m int) *OuterFECEncoder {
	return &OuterFECEncoder{n: n, m: m}
}

// Add wraps payload with an outer FEC header and returns it for immediate
// transmission.  When the Nth payload completes a group, M parity payloads
// are appended to the return slice so the caller can transmit them right after.
func (e *OuterFECEncoder) Add(payload []byte) ([][]byte, error) {
	idx := len(e.buf)
	if idx == 0 {
		e.currentGroup.Add(1)
		e.maxLen = 0
	}
	if len(payload) > e.maxLen {
		e.maxLen = len(payload)
	}

	cp := make([]byte, len(payload))
	copy(cp, payload)
	e.buf = append(e.buf, cp)

	gid := e.currentGroup.Load()
	result := make([][]byte, 0, 1+e.m)
	result = append(result, outerWrap(gid, uint8(e.n), uint8(e.m), uint8(idx), payload)) //nolint:gosec

	if len(e.buf) < e.n {
		return result, nil
	}

	parity, err := e.computeParity(gid)
	e.buf = e.buf[:0]
	e.maxLen = 0
	if err != nil {
		return result, fmt.Errorf("outer FEC encode parity: %w", err)
	}
	result = append(result, parity...)
	return result, nil
}

func (e *OuterFECEncoder) computeParity(gid uint32) ([][]byte, error) {
	shardSize := e.maxLen + 2
	shards := make([][]byte, e.n+e.m)
	for i, p := range e.buf {
		s := make([]byte, shardSize)
		binary.BigEndian.PutUint16(s, uint16(len(p))) //nolint:gosec
		copy(s[2:], p)
		shards[i] = s
	}
	for i := e.n; i < e.n+e.m; i++ {
		shards[i] = make([]byte, shardSize)
	}

	enc, err := reedsolomon.New(e.n, e.m)
	if err != nil {
		return nil, fmt.Errorf("outer RS init: %w", err)
	}
	if err := enc.Encode(shards); err != nil {
		return nil, fmt.Errorf("outer RS encode: %w", err)
	}

	result := make([][]byte, e.m)
	for i := range e.m {
		result[i] = outerWrap(gid, uint8(e.n), uint8(e.m), uint8(e.n+i), shards[e.n+i]) //nolint:gosec
	}
	return result, nil
}

// outerWrap prepends the 8-byte outer FEC header to payload and returns the
// combined slice.
func outerWrap(groupID uint32, n, m, index uint8, payload []byte) []byte {
	out := make([]byte, outerHeaderSize+len(payload))
	out[0] = outerMagicByte
	binary.BigEndian.PutUint32(out[1:5], groupID)
	out[5] = n
	out[6] = m
	out[7] = index
	copy(out[outerHeaderSize:], payload)
	return out
}

// parseOuterHeader extracts the outer FEC header from a wrapped payload.
// Returns ok=false if the slice is too short or the magic byte is wrong.
func parseOuterHeader(wrapped []byte) (uint32, int, int, int, []byte, bool) {
	if len(wrapped) < outerHeaderSize || wrapped[0] != outerMagicByte {
		return 0, 0, 0, 0, nil, false
	}
	groupID := binary.BigEndian.Uint32(wrapped[1:5])
	n := int(wrapped[5])
	m := int(wrapped[6])
	index := int(wrapped[7])
	payload := wrapped[outerHeaderSize:]
	return groupID, n, m, index, payload, true
}

// ─── Decoder ──────────────────────────────────────────────────────────────────

// OuterStats holds cumulative counters exported by OuterFECDecoder.Stats.
type OuterStats struct {
	FramesLost      int64
	FramesRecovered int64
	GroupsCompleted int64
	GroupsAbandoned int64
}

// OuterFECDecoder strips outer FEC headers, delivers data payloads immediately,
// and recovers missed data payloads once enough parity shards arrive.
// Safe for concurrent use.
type OuterFECDecoder struct {
	mu      sync.Mutex
	groups  map[uint32]*outerGroup
	timeout time.Duration
	now     func() time.Time

	framesLost      atomic.Int64
	framesRecovered atomic.Int64
	groupsCompleted atomic.Int64
	groupsAbandoned atomic.Int64
}

type outerGroup struct {
	n, m         int
	shardSize    int      // padded shard size (len of parity payload); 0 = unknown
	data         [][]byte // [n] original data payloads; nil = missing
	parityShards [][]byte // [m] parity shards; nil = not received
	delivered    []bool   // [n] which data payloads were already delivered
	firstSeen    time.Time
}

const defaultOuterGroupTimeout = 30 * time.Second

// NewOuterFECDecoder creates a decoder. The n and m parameters are accepted for
// API symmetry with NewOuterFECEncoder; the decoder reads N and M from the
// wire-format header of each received payload and does not need them upfront.
func NewOuterFECDecoder(_, _ int) *OuterFECDecoder {
	return &OuterFECDecoder{
		groups:  make(map[uint32]*outerGroup),
		timeout: defaultOuterGroupTimeout,
		now:     time.Now,
	}
}

// Accept processes one outer-FEC-wrapped payload.  Returns the application
// payloads that can be delivered now: the data payload itself (for data
// indices) or any previously missing data payloads recovered via RS (for
// parity indices).
func (d *OuterFECDecoder) Accept(wrapped []byte) ([][]byte, error) {
	gid, n, m, idx, payload, ok := parseOuterHeader(wrapped)
	if !ok {
		return nil, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.sweepExpiredLocked()
	g := d.getOrCreate(gid, n, m)

	var results [][]byte
	if idx < n {
		results = d.acceptData(g, idx, payload)
	} else {
		results = d.acceptParity(g, idx-n, payload)
	}

	d.maybeCloseGroup(gid, g)
	return results, nil
}

// getOrCreate returns or initialises the outerGroup for gid.
// Called with d.mu held.
func (d *OuterFECDecoder) getOrCreate(gid uint32, n, m int) *outerGroup {
	g, exists := d.groups[gid]
	if !exists {
		g = &outerGroup{
			n:            n,
			m:            m,
			data:         make([][]byte, n),
			parityShards: make([][]byte, m),
			delivered:    make([]bool, n),
			firstSeen:    d.now(),
		}
		d.groups[gid] = g
	}
	return g
}

// acceptData records the data payload, delivers it immediately (once), and
// returns it as a single-element slice.  Called with d.mu held.
func (d *OuterFECDecoder) acceptData(g *outerGroup, idx int, payload []byte) [][]byte {
	if g.data[idx] == nil {
		cp := make([]byte, len(payload))
		copy(cp, payload)
		g.data[idx] = cp
	}
	if g.delivered[idx] {
		return nil
	}
	g.delivered[idx] = true
	cp := make([]byte, len(payload))
	copy(cp, payload)
	return [][]byte{cp}
}

// acceptParity records the parity shard and attempts RS recovery of any
// missing data payloads.  Called with d.mu held.
func (d *OuterFECDecoder) acceptParity(g *outerGroup, pi int, payload []byte) [][]byte {
	if g.parityShards[pi] == nil {
		cp := make([]byte, len(payload))
		copy(cp, payload)
		g.parityShards[pi] = cp
		if g.shardSize == 0 {
			g.shardSize = len(payload)
		}
	}
	return d.tryRecover(g)
}

// maybeCloseGroup removes the group if all data payloads have been delivered.
// Called with d.mu held.
func (d *OuterFECDecoder) maybeCloseGroup(gid uint32, g *outerGroup) {
	for _, del := range g.delivered {
		if !del {
			return
		}
	}
	delete(d.groups, gid)
	d.groupsCompleted.Add(1)
}

// tryRecover attempts RS reconstruction of missing data shards using the
// received data and parity shards.  Returns newly recovered payloads.
// Called with d.mu held.
func (d *OuterFECDecoder) tryRecover(g *outerGroup) [][]byte {
	if !d.canRecover(g) {
		return nil
	}
	shards := d.buildRSShards(g)
	enc, err := reedsolomon.New(g.n, g.m)
	if err != nil {
		return nil
	}
	if err := enc.ReconstructData(shards); err != nil {
		return nil
	}
	return d.collectRecovered(g, shards)
}

// canRecover returns true if we have shardSize (parity received) and at least
// n total shards available (data + parity).  Called with d.mu held.
func (d *OuterFECDecoder) canRecover(g *outerGroup) bool {
	if g.shardSize == 0 {
		return false
	}
	received := 0
	for _, p := range g.data {
		if p != nil {
			received++
		}
	}
	for _, ps := range g.parityShards {
		if ps != nil {
			received++
		}
	}
	return received >= g.n
}

// buildRSShards assembles the n+m shard slice for the RS decoder.
// Received data shards are padded to g.shardSize; missing shards are nil.
// Called with d.mu held.
func (d *OuterFECDecoder) buildRSShards(g *outerGroup) [][]byte {
	shards := make([][]byte, g.n+g.m)
	for i, p := range g.data {
		if p == nil {
			continue
		}
		s := make([]byte, g.shardSize)
		binary.BigEndian.PutUint16(s, uint16(len(p))) //nolint:gosec
		copy(s[2:], p)
		shards[i] = s
	}
	for i, ps := range g.parityShards {
		if ps != nil {
			shards[g.n+i] = append([]byte(nil), ps...)
		}
	}
	return shards
}

// collectRecovered extracts recovered application payloads from the
// reconstructed shard array, updates group state, and increments metrics.
// Called with d.mu held.
func (d *OuterFECDecoder) collectRecovered(g *outerGroup, shards [][]byte) [][]byte {
	var results [][]byte
	for i := range g.n {
		if g.data[i] != nil || g.delivered[i] {
			continue
		}
		s := shards[i]
		if len(s) < 2 {
			continue
		}
		origLen := int(binary.BigEndian.Uint16(s))
		if origLen > len(s)-2 {
			continue
		}
		cp := make([]byte, origLen)
		copy(cp, s[2:2+origLen])
		g.data[i] = cp
		g.delivered[i] = true
		results = append(results, cp)
		d.framesRecovered.Add(1)
	}
	return results
}

// SweepExpired removes groups older than the configured timeout and updates
// loss metrics for any data payloads that were never delivered.
// Returns the number of groups removed.
func (d *OuterFECDecoder) SweepExpired() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sweepExpiredLocked()
}

// sweepExpiredLocked is the lock-held variant of SweepExpired.
func (d *OuterFECDecoder) sweepExpiredLocked() int {
	now := d.now()
	removed := 0
	for gid, g := range d.groups {
		if now.Sub(g.firstSeen) <= d.timeout {
			continue
		}
		lost := 0
		for _, del := range g.delivered {
			if !del {
				lost++
			}
		}
		if lost > 0 {
			d.framesLost.Add(int64(lost))
			d.groupsAbandoned.Add(1)
		} else {
			d.groupsCompleted.Add(1)
		}
		delete(d.groups, gid)
		removed++
	}
	return removed
}

// Stats returns a point-in-time snapshot of cumulative outer FEC counters.
func (d *OuterFECDecoder) Stats() OuterStats {
	return OuterStats{
		FramesLost:      d.framesLost.Load(),
		FramesRecovered: d.framesRecovered.Load(),
		GroupsCompleted: d.groupsCompleted.Load(),
		GroupsAbandoned: d.groupsAbandoned.Load(),
	}
}
