package transport

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/openlibrecommunity/olcrtc/internal/visual/codec"
	"github.com/openlibrecommunity/olcrtc/internal/visual/fec"
)

// maxSlotPayload is the largest application byte slice we can put inside
// one slot *after* the 13-byte header. It is derived from the codec's
// per-frame capacity so higher layers do not have to know codec internals.
const maxSlotPayload = codec.MaxPayloadBytes - HeaderSize // 297 - 13 = 284

// ErrMessageTooLarge is returned when Fragment is called with a payload
// that cannot fit in a single Reed-Solomon block under the configured
// parity. Callers upstream of Fragmenter are expected to have split their
// data to ≤ Jazz's maxDataChannelMessageSize first.
var ErrMessageTooLarge = errors.New("transport: message exceeds fragmenter capacity")

var errRSBlockTooWide = errors.New("transport: RS block exceeds field width")

// Fragmenter turns an application message into a sequence of wire slots.
//
// One Fragment() call produces N+M slots for exactly one message ID. The
// number of data shards N is chosen so that each shard fits in one slot;
// the parity shard count M comes from the configured redundancy.
//
// The type is safe for concurrent use — state is limited to an atomic
// message counter.
type Fragmenter struct {
	// parityRatio is the fraction of parity shards relative to data
	// shards, expressed as a percent (40 = 40% redundancy). We clamp to
	// at least one parity shard per message so even a single-slot
	// message has basic recovery capability.
	parityRatio int

	// nextMsgID is atomically incremented per Fragment call. uint32
	// wrap-around is fine: the Reassembler only uses it as a key in a
	// short-lived map with a timeout, not as a sequence counter.
	nextMsgID atomic.Uint32
}

// NewFragmenter creates a fragmenter with a given parity ratio percent.
// Pass 40 for the plan default (10 data + 4 parity).
func NewFragmenter(parityRatioPercent int) *Fragmenter {
	if parityRatioPercent < 0 {
		parityRatioPercent = 0
	}
	return &Fragmenter{parityRatio: parityRatioPercent}
}

// Fragment splits payload into slots. Each returned slot is a []byte of
// length HeaderSize + shardSize ≤ codec.MaxPayloadBytes, ready to be
// passed directly to codec.Encode.
//
// The returned slice contains N + M slots in the order
//
//	slot[0]     = data shard 0
//	...
//	slot[N-1]   = data shard N-1
//	slot[N]     = parity shard 0
//	...
//	slot[N+M-1] = parity shard M-1
//
// Callers are free to reorder or shuffle them (the header carries an
// explicit index) but must send all N+M to maximise the reassembly
// probability.
func (f *Fragmenter) Fragment(payload []byte) ([][]byte, error) {
	// Choose N so that the resulting shard size fits in a slot after
	// the header is added. fec.ShardSize adds 4 bytes for the FEC
	// length prefix, so we must ensure (len+4)/N ≤ maxSlotPayload.
	n := chooseDataShardCount(len(payload))
	if n == 0 {
		return nil, ErrMessageTooLarge
	}

	m := parityCount(n, f.parityRatio)
	if n+m > maxRSTotal {
		return nil, fmt.Errorf("%w: n=%d m=%d", errRSBlockTooWide, n, m)
	}

	encoder, err := fec.NewEncoder(n, m)
	if err != nil {
		return nil, fmt.Errorf("transport: build FEC encoder: %w", err)
	}

	shards, err := encoder.Encode(payload)
	if err != nil {
		return nil, fmt.Errorf("transport: FEC encode: %w", err)
	}

	msgID := f.nextMsgID.Add(1)
	total := len(shards)
	slots := make([][]byte, total)

	for i, shard := range shards {
		slots[i] = packSlot(Header{
			MsgID: msgID,
			N:     uint16(n), //nolint:gosec // n is bounded by maxRSTotal (256)
			M:     uint8(m),  //nolint:gosec // m is bounded by maxRSTotal (256)
			Index: uint16(i),
		}, shard)
	}

	return slots, nil
}

// chooseDataShardCount picks the smallest N for which
// fec.ShardSize(payloadLen) ≤ maxSlotPayload. In practice this is
// ceil((payloadLen + 4) / maxSlotPayload) and ≥ 1.
//
// We iterate from 1 up — the correct answer is usually 1–50 and the math
// is exact, so a closed-form expression is overkill and would hide the
// invariant we actually care about.
func chooseDataShardCount(payloadLen int) int {
	if payloadLen == 0 {
		// fec.ShardSize(0) returns 1, so a single data shard is
		// legal for an empty application message.
		return 1
	}
	// Fast path: compute a lower bound assuming shardSize = maxSlotPayload.
	n := (payloadLen + 4 + maxSlotPayload - 1) / maxSlotPayload
	if n < 1 {
		n = 1
	}
	// Walk upward if the rounding pushed us over the limit.
	for n <= maxRSTotal {
		shardSize := fec.ShardSizeFor(payloadLen, n)
		if shardSize <= maxSlotPayload {
			return n
		}
		n++
	}
	return 0
}

// parityCount applies the configured parity ratio to a given N, with a
// minimum of 1 parity shard so every message can tolerate at least one
// slot loss.
func parityCount(n, ratioPercent int) int {
	m := (n*ratioPercent + 99) / 100
	if m < 1 {
		m = 1
	}
	return m
}

// maxRSTotal is the Reed-Solomon field limit (n+m ≤ 256). We reserve it
// here to keep constraints local.
const maxRSTotal = 256
