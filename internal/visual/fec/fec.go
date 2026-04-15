// Package fec implements the inner forward-error-correction layer for the
// visual transport.
//
// # Why inner FEC
//
// The visual codec (internal/visual/codec) can recover small per-pixel luma
// drifts by bucket snapping, but it cannot recover whole-block bit flips
// that a downstream video codec may introduce after requantisation. For that
// we need a classical FEC: encode N data shards into N+M parity shards,
// such that any N out of (N+M) surviving shards can reconstruct the
// originals.
//
// Reed-Solomon is the obvious fit: it is MDS (best possible recovery), has
// a pure-Go implementation (klauspost/reedsolomon), and supports shards
// down to a few bytes each without pathological CPU overhead.
//
// # Scheme
//
// We take a byte slice of arbitrary length, pad it to a multiple of N and
// split into N "data" shards of equal size. Reed-Solomon produces M extra
// "parity" shards. All N+M shards are emitted as a flat slice-of-slices;
// the caller is responsible for packing them into frames and recovering
// which indices survived on the receiving side.
//
// To make the round trip symmetric we also serialise the *original byte
// length* alongside the shards (prefixed on shard 0). Without it the
// decoder cannot tell padding zeros from legitimate trailing zero bytes.
//
// # Parameters
//
// The default (N=10, M=4) = 40% overhead is the plan's target. It tolerates
// losing up to 4 arbitrary shards out of every 14. That is a solid fit for
// a visual channel where a single re-encoded frame tends to damage a whole
// macroblock row but rarely more than that.
//
// # Threading
//
// Encoders are safe for concurrent use only if you do not share the shard
// buffers between goroutines; the underlying reedsolomon.Encoder is itself
// stateless after New so we just wrap it.
package fec

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/klauspost/reedsolomon"
)

const (
	// lengthPrefixBytes is the number of bytes reserved at the start of
	// shard 0 to hold the uint32 original payload length. uint32 covers
	// any reasonable frame payload and leaves room to grow if we ever
	// bundle multiple application messages per FEC block.
	lengthPrefixBytes = 4
)

var (
	errInvalidGeometry         = errors.New("fec: n and m must be positive")
	errFieldSizeExceeded       = errors.New("fec: n+m exceeds Reed-Solomon field size")
	errUnexpectedShardCount    = errors.New("fec: unexpected shard count")
	errBufferTooSmall          = errors.New("fec: reconstructed buffer too small for length prefix")
	errInvalidReconstructedLen = errors.New("fec: reconstructed length prefix exceeds data buffer")
	errPayloadTooLarge         = errors.New("fec: payload too large for uint32 length prefix")
)

// Encoder wraps a Reed-Solomon coder configured with fixed (n, m) shards.
// It is cheap to construct and holds no per-call state, so callers can
// instantiate one per transport peer.
type Encoder struct {
	n int // number of data shards
	m int // number of parity shards
	// rs is the underlying klauspost encoder. Its Split/Encode/Reconstruct
	// methods are independent, so we do not guard it with a mutex.
	rs reedsolomon.Encoder
}

// NewEncoder builds an FEC encoder. n must be ≥ 1, m ≥ 1, and n+m ≤ 256
// (Reed-Solomon field limit). Callers should stick to the plan defaults
// (10, 4) unless measurements justify a different ratio.
func NewEncoder(n, m int) (*Encoder, error) {
	if n < 1 || m < 1 {
		return nil, errInvalidGeometry
	}
	if n+m > 256 {
		return nil, errFieldSizeExceeded
	}
	rs, err := reedsolomon.New(n, m)
	if err != nil {
		return nil, fmt.Errorf("fec: build RS encoder: %w", err)
	}
	return &Encoder{n: n, m: m, rs: rs}, nil
}

// N returns the configured number of data shards.
func (e *Encoder) N() int { return e.n }

// M returns the configured number of parity shards.
func (e *Encoder) M() int { return e.m }

// Total returns n+m, i.e. the total number of shards emitted per Encode call.
func (e *Encoder) Total() int { return e.n + e.m }

// ShardSize computes the per-shard byte size that Encode will produce for
// an input of the given byte length. Callers can use it to pre-size frame
// buffers without going through an actual Encode round.
func (e *Encoder) ShardSize(payloadLen int) int {
	return ShardSizeFor(payloadLen, e.n)
}

// ShardSizeFor computes the per-shard byte size for an (n) data-shard
// Reed-Solomon block without needing an Encoder instance. Useful for the
// transport layer, which wants to pick n before constructing the encoder
// so that the shard fits in a wire slot.
func ShardSizeFor(payloadLen, n int) int {
	if n < 1 {
		return 0
	}
	// Add lengthPrefixBytes for the uint32 length header stored on
	// shard 0, then round up to a multiple of n.
	total := payloadLen + lengthPrefixBytes
	ss := total / n
	if total%n != 0 {
		ss++
	}
	if ss == 0 {
		ss = 1 // Reed-Solomon rejects zero-sized shards.
	}
	return ss
}

// Encode packs payload into exactly n+m equal-length shards. The first
// lengthPrefixBytes bytes of shard 0 carry the original payload length as a
// big-endian uint32; the remainder is the payload data followed by zero
// padding up to n*shardSize. Parity shards are then computed over all n
// data shards.
//
// The returned slice is freshly allocated and owned by the caller.
func (e *Encoder) Encode(payload []byte) ([][]byte, error) {
	if len(payload) > int(^uint32(0)) {
		return nil, fmt.Errorf("%w: len=%d", errPayloadTooLarge, len(payload))
	}
	shardSize := e.ShardSize(len(payload))
	dataSize := e.n * shardSize

	// Build the flat data buffer: [len:4][payload:len][pad:?].
	flat := make([]byte, dataSize)
	//nolint:gosec // guarded above by explicit uint32 bound check
	binary.BigEndian.PutUint32(flat[:lengthPrefixBytes], uint32(len(payload)))
	copy(flat[lengthPrefixBytes:], payload)

	// Slice the flat buffer into n data shards. We do not copy — the
	// parity computation below writes into fresh parity slices.
	shards := make([][]byte, e.Total())
	for i := range e.n {
		shards[i] = flat[i*shardSize : (i+1)*shardSize]
	}
	for i := e.n; i < e.Total(); i++ {
		shards[i] = make([]byte, shardSize)
	}

	if err := e.rs.Encode(shards); err != nil {
		return nil, fmt.Errorf("fec: RS encode: %w", err)
	}
	return shards, nil
}

// Decode attempts to reconstruct the original payload from a partial set of
// shards. Lost shards must be passed as nil slices of the same length as
// surviving shards; the caller is responsible for tracking which indices
// went missing.
//
// The shards slice must contain exactly n+m entries (the caller must know
// the Encoder geometry — Decode does not auto-detect). Up to m shards may
// be nil; any more and ErrTooManyLost is returned.
func (e *Encoder) Decode(shards [][]byte) ([]byte, error) {
	if len(shards) != e.Total() {
		return nil, fmt.Errorf("%w: expected %d shards, got %d", errUnexpectedShardCount, e.Total(), len(shards))
	}

	// Count missing and reject catastrophic cases up front with a typed
	// error so the caller can distinguish "codec broken" from "too much
	// loss this frame".
	missing := 0
	for _, s := range shards {
		if s == nil {
			missing++
		}
	}
	if missing > e.m {
		return nil, ErrTooManyLost
	}

	if err := e.rs.Reconstruct(shards); err != nil {
		return nil, fmt.Errorf("fec: RS reconstruct: %w", err)
	}

	// Reassemble data shards back into the flat buffer and read the
	// length prefix to trim padding.
	shardSize := len(shards[0])
	flat := make([]byte, e.n*shardSize)
	for i := range e.n {
		copy(flat[i*shardSize:(i+1)*shardSize], shards[i])
	}

	if len(flat) < lengthPrefixBytes {
		return nil, errBufferTooSmall
	}
	length := binary.BigEndian.Uint32(flat[:lengthPrefixBytes])
	if int(length) > len(flat)-lengthPrefixBytes {
		return nil, fmt.Errorf("%w: prefix=%d buffer=%d", errInvalidReconstructedLen, length, len(flat)-lengthPrefixBytes)
	}

	// Copy to a fresh slice so the caller does not accidentally hold onto
	// the underlying shard storage.
	out := make([]byte, int(length))
	copy(out, flat[lengthPrefixBytes:lengthPrefixBytes+int(length)])
	return out, nil
}

// ErrTooManyLost is returned when the number of missing shards exceeds the
// parity count, i.e. Reed-Solomon cannot recover. Callers should surface
// this as a per-frame drop and rely on the outer interleave layer to fill
// the gap.
var ErrTooManyLost = errors.New("fec: too many shards lost to reconstruct")
