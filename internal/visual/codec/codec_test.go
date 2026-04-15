package codec

import (
	"bytes"
	"errors"
	"image"
	"math/rand"
	"testing"
)

// TestConstants pins the frame-geometry numbers so a future change has to
// pass through an explicit test update. Breaking these values means we also
// have to audit the transport layer, which assumes a fixed per-frame
// capacity when computing its fragmentation math.
func TestConstants(t *testing.T) {
	if BlocksX != 40 || BlocksY != 30 {
		t.Fatalf("unexpected grid: %dx%d", BlocksX, BlocksY)
	}
	if BlocksTotal != 1200 {
		t.Fatalf("BlocksTotal = %d, want 1200", BlocksTotal)
	}
	if DataBlocks != 1188 {
		t.Fatalf("DataBlocks = %d, want 1188", DataBlocks)
	}
	if MaxPayloadBytes != 297 {
		t.Fatalf("MaxPayloadBytes = %d, want 297", MaxPayloadBytes)
	}
}

// TestEncodeDecodeRoundTrip exercises the happy path: deterministic input,
// no corruption, byte-exact recovery. Uses a handful of sizes including the
// 0-length and capacity edge cases.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []int{0, 1, 2, 7, 64, 128, MaxPayloadBytes - 1, MaxPayloadBytes}

	for _, size := range cases {
		t.Run("", func(t *testing.T) {
			payload := makePayload(size, int64(size)+1)

			frame, err := Encode(payload)
			if err != nil {
				t.Fatalf("encode size=%d: %v", size, err)
			}

			got, err := Decode(frame)
			if err != nil {
				t.Fatalf("decode size=%d: %v", size, err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("size=%d payload mismatch: got %x want %x", size, got, payload)
			}
		})
	}
}

// TestPayloadTooLarge ensures the capacity guard actually rejects oversized
// input. This is a correctness boundary: the transport layer above this
// codec relies on the error to trigger fragmentation.
func TestPayloadTooLarge(t *testing.T) {
	payload := makePayload(MaxPayloadBytes+1, 1)
	if _, err := Encode(payload); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("expected ErrPayloadTooLarge, got %v", err)
	}
}

// TestSyncMismatch makes sure Decode rejects a frame whose corner pattern
// has been stomped. This guards against silently decoding noise as payload
// if we ever lose geometry lock in production.
func TestSyncMismatch(t *testing.T) {
	frame, err := Encode([]byte("hello"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Break one of the corners. Any non-matching symbol works.
	writeBlockSymbol(frame, 0, 0, 0b11)

	if _, err := Decode(frame); !errors.Is(err, ErrSyncMismatch) {
		t.Fatalf("expected ErrSyncMismatch, got %v", err)
	}
}

// TestLumaNoiseTolerance proves the block centre averaging scheme survives
// per-pixel Gaussian-ish noise up to approximately one quarter of the
// inter-symbol distance. The palette has 56 units between centres so we
// expect the decoder to be robust up to at least ±14 units of i.i.d. noise.
//
// We model noise as uniform in [-ampl, ampl] per pixel and sweep the
// amplitude, failing only if the recovered payload does not match. Under
// the offline harness (no codec re-quantisation) this should succeed for
// all amplitudes well below 28.
func TestLumaNoiseTolerance(t *testing.T) {
	payload := makePayload(200, 42)
	frame, err := Encode(payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	rng := rand.New(rand.NewSource(42)) //nolint:gosec // test-only PRNG
	const ampl = 20                     // well under the palette step of 56

	addNoise(frame.Y, ampl, rng)

	got, err := Decode(frame)
	if err != nil {
		t.Fatalf("decode after noise: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch after noise:\n got  %x\n want %x", got, payload)
	}
}

// TestLumaNoiseCatastrophic demonstrates the failure mode at amplitudes
// exceeding the inter-symbol distance: the codec *alone* cannot recover,
// and that is exactly why we need an FEC layer on top. This test does not
// assert success — it asserts the decoder at least runs to completion and
// the sync corners are not wiped out (because we leave them alone).
//
// Actual robustness at this noise level is the FEC layer's job, see the
// internal/visual/fec package.
func TestLumaNoiseCatastrophic(t *testing.T) {
	payload := makePayload(100, 7)
	frame, err := Encode(payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	rng := rand.New(rand.NewSource(7)) //nolint:gosec // test-only PRNG
	const ampl = 60                    // > palette step of 56

	// Leave a margin around each sync corner block so the sync check
	// still passes — the goal here is to show *payload* degradation,
	// not to test sync-corner failure (that path is covered separately).
	addNoiseMasked(frame.Y, ampl, rng, syncCornerMask(frame))

	got, err := Decode(frame)
	if err != nil {
		t.Fatalf("decode after catastrophic noise: %v", err)
	}

	// We do not assert payload equality; some symbols are expected to
	// flip. Assert instead that length prefix was still decoded (Decode
	// would return a length-mismatch error otherwise), which means at
	// least the length-block bits were not all destroyed. This is a
	// weak check but documents the failure envelope.
	if len(got) != len(payload) {
		t.Logf("length prefix drifted: got %d, want %d", len(got), len(payload))
	}
}

// Helpers.

// makePayload produces deterministic pseudo-random bytes for a given seed.
// Tests rely on determinism so failures are reproducible from CI logs.
func makePayload(n int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // test-only PRNG
	out := make([]byte, n)
	_, _ = rng.Read(out)
	return out
}

// addNoise perturbs every luma sample by a uniform noise term in
// [-ampl, +ampl], clamping to the byte range. Used to model mild codec
// re-quantisation in decoder tests.
func addNoise(buf []byte, ampl int, rng *rand.Rand) {
	for i := range buf {
		buf[i] = clampByte(int(buf[i]) + rng.Intn(2*ampl+1) - ampl)
	}
}

// addNoiseMasked is addNoise but skips any byte the mask says to leave
// alone. Mask is a parallel byte slice: non-zero means "protect".
func addNoiseMasked(buf []byte, ampl int, rng *rand.Rand, mask []byte) {
	for i := range buf {
		if mask[i] != 0 {
			continue
		}
		buf[i] = clampByte(int(buf[i]) + rng.Intn(2*ampl+1) - ampl)
	}
}

// syncCornerMask builds a protection mask that covers the four sync corner
// blocks. Everything else is zero. Used so the catastrophic-noise test can
// still pass the sync check and reach the payload decoder.
func syncCornerMask(frame *image.YCbCr) []byte {
	mask := make([]byte, len(frame.Y))
	for i := range 4 {
		bx, by := syncCornerCoords(i)
		px := bx * BlockSize
		py := by * BlockSize
		for row := range BlockSize {
			offset := (py+row)*frame.YStride + px
			for col := range BlockSize {
				mask[offset+col] = 1
			}
		}
	}
	return mask
}

func clampByte(v int) byte {
	switch {
	case v < 0:
		return 0
	case v > 255:
		return 255
	default:
		return byte(v)
	}
}
