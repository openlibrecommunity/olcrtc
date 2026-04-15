package jazz

import (
	"bytes"
	"math/rand"
	"testing"
	"time"
)

func TestVisualEngineLocalLoopback(t *testing.T) {
	src := newVisualEngine()
	dst := newVisualEngine()

	var got []byte
	dst.setOnPayload(func(payload []byte) {
		got = append([]byte(nil), payload...)
	})

	src.startFramePump()

	payload := makeVisualPayload(1200, 11)
	if err := src.queueVisualPayload(payload); err != nil {
		t.Fatalf("queueVisualPayload: %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for len(got) == 0 && time.Now().Before(deadline) {
		frame, ok := src.waitOutboundFrame(50 * time.Millisecond)
		if !ok {
			continue
		}
		if err := dst.acceptDecodedFrame(frame); err != nil {
			t.Fatalf("acceptDecodedFrame: %v", err)
		}
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch:\n got  %x\n want %x", got, payload)
	}
}

func TestVisualEngineLocalLoopbackNoiseAndFrameLoss(t *testing.T) {
	src := newVisualEngine()
	dst := newVisualEngine()

	var got []byte
	dst.setOnPayload(func(payload []byte) {
		got = append([]byte(nil), payload...)
	})

	src.startFramePump()

	payload := makeVisualPayload(900, 12)
	if err := src.queueVisualPayload(payload); err != nil {
		t.Fatalf("queueVisualPayload: %v", err)
	}

	rng := rand.New(rand.NewSource(12)) //nolint:gosec // test-only PRNG
	i := 0
	deadline := time.Now().Add(500 * time.Millisecond)
	for len(got) == 0 && time.Now().Before(deadline) {
		frame, ok := src.waitOutboundFrame(50 * time.Millisecond)
		if !ok {
			continue
		}
		if i == 0 {
			i++
			continue // simulate one lost frame
		}
		addVisualNoise(frame.Y, 12, rng)
		if err := dst.acceptDecodedFrame(frame); err != nil {
			t.Fatalf("acceptDecodedFrame[%d]: %v", i, err)
		}
		i++
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch:\n got  %x\n want %x", got, payload)
	}
}

func makeVisualPayload(n int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // test-only PRNG
	out := make([]byte, n)
	_, _ = rng.Read(out)
	return out
}

func addVisualNoise(buf []byte, ampl int, rng *rand.Rand) {
	for i := range buf {
		buf[i] = clampVisualByte(int(buf[i]) + rng.Intn(2*ampl+1) - ampl)
	}
}

func clampVisualByte(v int) byte {
	switch {
	case v < 0:
		return 0
	case v > 255:
		return 255
	default:
		return byte(v)
	}
}
