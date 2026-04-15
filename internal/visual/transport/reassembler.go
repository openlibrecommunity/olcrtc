package transport

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/visual/fec"
)

// ErrSlotInconsistent is returned when a slot claims geometry that does not
// match the message state already accumulated under the same message id.
var ErrSlotInconsistent = errors.New("transport: slot geometry inconsistent with message state")

var (
	errInvalidRSGeometry = errors.New("transport: invalid RS geometry")
	errSlotIndexRange    = errors.New("transport: slot index out of range")
)

// Reassembler collects slots produced by Fragmenter and reconstructs the
// original application payload once enough shards have arrived.
//
// Invalid slots (bad magic/version/CRC) are silently dropped: on a real
// visual link this is the normal corruption path, not an exceptional one.
// Structural contradictions inside an otherwise valid slot return an error.
type Reassembler struct {
	mu      sync.Mutex
	msgs    map[uint32]*partialMessage
	timeout time.Duration
	now     func() time.Time
}

type partialMessage struct {
	n         int
	m         int
	shardSize int
	shards    [][]byte
	received  []bool
	count     int
	lastSeen  time.Time
}

const defaultMessageTimeout = 30 * time.Second

// NewReassembler creates an empty slot collector.
func NewReassembler() *Reassembler {
	return NewReassemblerWithTimeout(defaultMessageTimeout)
}

// NewReassemblerWithTimeout creates an empty slot collector with a custom
// idle timeout for partially received messages.
func NewReassemblerWithTimeout(timeout time.Duration) *Reassembler {
	if timeout <= 0 {
		timeout = defaultMessageTimeout
	}
	return &Reassembler{
		msgs:    make(map[uint32]*partialMessage),
		timeout: timeout,
		now:     time.Now,
	}
}

// Accept ingests one wire slot. It returns (payload, true, nil) once the
// message is fully reconstructed, (nil, false, nil) for a slot that was
// accepted but did not complete the message yet, and also (nil, false, nil)
// for a corrupted slot that was dropped due to bad magic/version/CRC.
//
//nolint:cyclop // validation + state transition are intentionally kept together
func (r *Reassembler) Accept(slot []byte) ([]byte, bool, error) {
	now := r.now()

	h, shard, ok := parseSlot(slot)
	if !ok {
		return nil, false, nil
	}

	total := int(h.Total())
	if h.N == 0 || h.M == 0 || total > maxRSTotal {
		return nil, false, fmt.Errorf("%w: n=%d m=%d", errInvalidRSGeometry, h.N, h.M)
	}
	if int(h.Index) >= total {
		return nil, false, fmt.Errorf("%w: index=%d total=%d", errSlotIndexRange, h.Index, total)
	}

	r.mu.Lock()
	r.sweepExpiredLocked(now)
	pm, exists := r.msgs[h.MsgID]
	if !exists {
		pm = &partialMessage{
			n:         int(h.N),
			m:         int(h.M),
			shardSize: len(shard),
			shards:    make([][]byte, total),
			received:  make([]bool, total),
			lastSeen:  now,
		}
		r.msgs[h.MsgID] = pm
	} else {
		if pm.n != int(h.N) || pm.m != int(h.M) || len(pm.shards) != total || pm.shardSize != len(shard) {
			r.mu.Unlock()
			return nil, false, ErrSlotInconsistent
		}
		pm.lastSeen = now
	}

	idx := int(h.Index)
	if !pm.received[idx] {
		pm.received[idx] = true
		pm.count++
		buf := make([]byte, len(shard))
		copy(buf, shard)
		pm.shards[idx] = buf
	}

	if pm.count < pm.n {
		r.mu.Unlock()
		return nil, false, nil
	}

	shards := make([][]byte, len(pm.shards))
	copy(shards, pm.shards)
	n, m, msgID := pm.n, pm.m, h.MsgID
	r.mu.Unlock()

	enc, err := fec.NewEncoder(n, m)
	if err != nil {
		return nil, false, fmt.Errorf("transport: build FEC decoder: %w", err)
	}
	payload, err := enc.Decode(shards)
	if errors.Is(err, fec.ErrTooManyLost) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("transport: FEC decode: %w", err)
	}

	r.mu.Lock()
	delete(r.msgs, msgID)
	r.mu.Unlock()
	return payload, true, nil
}

// SweepExpired drops any partial messages that have not seen a slot within the
// configured timeout. It returns the number of message buckets removed.
func (r *Reassembler) SweepExpired() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sweepExpiredLocked(r.now())
}

func (r *Reassembler) sweepExpiredLocked(now time.Time) int {
	if len(r.msgs) == 0 {
		return 0
	}
	removed := 0
	for msgID, pm := range r.msgs {
		if now.Sub(pm.lastSeen) > r.timeout {
			delete(r.msgs, msgID)
			removed++
		}
	}
	return removed
}
