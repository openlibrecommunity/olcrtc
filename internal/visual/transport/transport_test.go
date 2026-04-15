package transport

import (
	"bytes"
	"math/rand"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/visual/codec"
)

func TestFragmentReassembleRoundTrip(t *testing.T) {
	cases := []int{0, 1, 64, 284, 285, 512, 1024, 4096, 12288}

	for _, size := range cases {
		t.Run("", func(t *testing.T) {
			f := NewFragmenter(40)
			r := NewReassembler()
			payload := makePayload(size, int64(size)+11)

			slots, err := f.Fragment(payload)
			if err != nil {
				t.Fatalf("fragment size=%d: %v", size, err)
			}
			if len(slots) == 0 {
				t.Fatalf("fragment size=%d: no slots", size)
			}

			var got []byte
			for _, slot := range slots {
				out, done, err := r.Accept(slot)
				if err != nil {
					t.Fatalf("accept size=%d: %v", size, err)
				}
				if done {
					got = out
				}
			}

			if !bytes.Equal(got, payload) {
				t.Fatalf("size=%d payload mismatch: got %x want %x", size, got, payload)
			}
		})
	}
}

func TestFragmentCodecNoiseReassemble(t *testing.T) {
	f := NewFragmenter(40)
	r := NewReassembler()

	payload := makePayload(900, 42)
	slots, err := f.Fragment(payload)
	if err != nil {
		t.Fatalf("fragment: %v", err)
	}
	if len(slots) < 3 {
		t.Fatalf("expected multi-slot message, got %d slots", len(slots))
	}

	rng := rand.New(rand.NewSource(42)) //nolint:gosec // test-only PRNG
	var got []byte

	for i, slot := range slots {
		frame, err := codec.Encode(slot)
		if err != nil {
			t.Fatalf("codec encode slot %d: %v", i, err)
		}

		addNoise(frame.Y, 12, rng)

		decodedSlot, err := codec.Decode(frame)
		if err != nil {
			t.Fatalf("codec decode slot %d: %v", i, err)
		}

		// Simulate one post-codec corrupted slot. CRC should drop it and
		// Reed-Solomon parity should still recover the message.
		if i == 0 {
			decodedSlot[len(decodedSlot)-1] ^= 0x01
		}

		out, done, err := r.Accept(decodedSlot)
		if err != nil {
			t.Fatalf("accept slot %d: %v", i, err)
		}
		if done {
			got = out
		}
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch:\n got  %x\n want %x", got, payload)
	}
}

func TestSenderReceiverRoundTrip(t *testing.T) {
	s := NewSender(40)
	r := NewReceiver()

	payload := makePayload(1200, 77)
	frames, err := s.EncodeFrames(payload)
	if err != nil {
		t.Fatalf("EncodeFrames: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("EncodeFrames returned no frames")
	}

	var got []byte
	for _, frame := range frames {
		out, done, err := r.AcceptFrame(frame)
		if err != nil {
			t.Fatalf("AcceptFrame: %v", err)
		}
		if done {
			got = out
		}
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch:\n got  %x\n want %x", got, payload)
	}
}

func TestSenderReceiverNoiseAndFrameLoss(t *testing.T) {
	s := NewSender(40)
	r := NewReceiver()

	payload := makePayload(900, 88)
	frames, err := s.EncodeFrames(payload)
	if err != nil {
		t.Fatalf("EncodeFrames: %v", err)
	}
	if len(frames) < 3 {
		t.Fatalf("expected multi-frame message, got %d frames", len(frames))
	}

	rng := rand.New(rand.NewSource(88)) //nolint:gosec // test-only PRNG
	var got []byte

	for i, frame := range frames {
		// Simulate one lost frame: receiver never sees it.
		if i == 0 {
			continue
		}

		addNoise(frame.Y, 12, rng)

		out, done, err := r.AcceptFrame(frame)
		if err != nil {
			t.Fatalf("AcceptFrame[%d]: %v", i, err)
		}
		if done {
			got = out
		}
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch:\n got  %x\n want %x", got, payload)
	}
}

func TestReassemblerDropsExpiredPartialMessage(t *testing.T) {
	f := NewFragmenter(40)
	r := NewReassemblerWithTimeout(10 * time.Millisecond)

	now := time.Unix(100, 0)
	r.now = func() time.Time { return now }

	payload1 := makePayload(600, 1)
	slots1, err := f.Fragment(payload1)
	if err != nil {
		t.Fatalf("fragment payload1: %v", err)
	}
	if len(slots1) < 2 {
		t.Fatalf("expected fragmented payload1")
	}

	assertIncompleteAccept(t, r, slots1[0], "first partial accept")

	now = now.Add(11 * time.Millisecond)
	if removed := r.SweepExpired(); removed != 1 {
		t.Fatalf("SweepExpired removed %d, want 1", removed)
	}

	payload2 := makePayload(600, 2)
	slots2, err := f.Fragment(payload2)
	if err != nil {
		t.Fatalf("fragment payload2: %v", err)
	}

	var got []byte
	for _, slot := range slots2 {
		out, done, err := r.Accept(slot)
		if err != nil {
			t.Fatalf("accept payload2: %v", err)
		}
		if done {
			got = out
		}
	}

	if !bytes.Equal(got, payload2) {
		t.Fatalf("payload2 mismatch:\n got  %x\n want %x", got, payload2)
	}
}

func TestReassemblerRefreshesTimeoutOnNewSlot(t *testing.T) {
	f := NewFragmenter(40)
	r := NewReassemblerWithTimeout(10 * time.Millisecond)

	now := time.Unix(200, 0)
	r.now = func() time.Time { return now }

	payload := makePayload(700, 3)
	slots, err := f.Fragment(payload)
	if err != nil {
		t.Fatalf("fragment: %v", err)
	}
	if len(slots) < 3 {
		t.Fatalf("expected fragmented payload")
	}

	if _, done, err := r.Accept(slots[0]); err != nil || done {
		t.Fatalf("accept slot0: done=%v err=%v", done, err)
	}

	now = now.Add(8 * time.Millisecond)
	if _, done, err := r.Accept(slots[1]); err != nil || done {
		t.Fatalf("accept slot1: done=%v err=%v", done, err)
	}

	now = now.Add(8 * time.Millisecond)
	if removed := r.SweepExpired(); removed != 0 {
		t.Fatalf("SweepExpired removed %d, want 0", removed)
	}
}

func makePayload(n int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // test-only PRNG
	out := make([]byte, n)
	_, _ = rng.Read(out)
	return out
}

func addNoise(buf []byte, ampl int, rng *rand.Rand) {
	for i := range buf {
		buf[i] = clampByte(int(buf[i]) + rng.Intn(2*ampl+1) - ampl)
	}
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

func assertIncompleteAccept(t *testing.T, r *Reassembler, slot []byte, label string) {
	t.Helper()
	out, done, err := r.Accept(slot)
	if err != nil || done || out != nil {
		t.Fatalf("%s: out=%v done=%v err=%v", label, out, done, err)
	}
}
