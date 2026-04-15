package fec

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"
)

// TestRoundTripNoLoss is the smoke test: encode, pass every shard to
// Decode, expect the exact bytes back.
func TestRoundTripNoLoss(t *testing.T) {
	enc := mustDefaultEncoder(t)

	for _, size := range []int{1, 13, 100, 297, 1024} {
		payload := makePayload(size, int64(size))

		shards, err := enc.Encode(payload)
		if err != nil {
			t.Fatalf("encode size=%d: %v", size, err)
		}
		if len(shards) != 14 {
			t.Fatalf("expected 14 shards, got %d", len(shards))
		}

		got, err := enc.Decode(shards)
		if err != nil {
			t.Fatalf("decode size=%d: %v", size, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("size=%d mismatch", size)
		}
	}
}

// TestRoundTripLoseDataShards proves we can reconstruct the payload when
// the maximum allowed number of *data* shards are missing. This is the
// hardest case for RS because it actually requires the parity shards to
// do real work.
func TestRoundTripLoseDataShards(t *testing.T) {
	enc := mustDefaultEncoder(t)
	payload := makePayload(250, 1)

	shards, err := enc.Encode(payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Drop the first m=4 data shards. Worst-case missing pattern for
	// "data only": forces reconstruction to pull all four parity shards.
	shards[0] = nil
	shards[1] = nil
	shards[2] = nil
	shards[3] = nil

	got, err := enc.Decode(shards)
	if err != nil {
		t.Fatalf("decode after losing 4 data shards: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch")
	}
}

// TestRoundTripLoseParityShards mirrors the above but drops parity. Should
// be trivial for RS — the data shards are intact — but we verify that
// Decode still works when surviving shards are only data.
func TestRoundTripLoseParityShards(t *testing.T) {
	enc := mustDefaultEncoder(t)
	payload := makePayload(250, 2)

	shards, err := enc.Encode(payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Drop all parity shards. Decoder must not require them.
	for i := 10; i < 14; i++ {
		shards[i] = nil
	}

	got, err := enc.Decode(shards)
	if err != nil {
		t.Fatalf("decode after losing parity: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch")
	}
}

// TestRoundTripLoseMixed drops a randomised combination of exactly m
// shards from anywhere in the set and checks recovery. The seed is fixed
// so failures are reproducible.
func TestRoundTripLoseMixed(t *testing.T) {
	enc := mustDefaultEncoder(t)
	payload := makePayload(200, 3)
	rng := rand.New(rand.NewSource(99)) //nolint:gosec // test-only

	for trial := range 20 {
		shards, err := enc.Encode(payload)
		if err != nil {
			t.Fatalf("encode trial=%d: %v", trial, err)
		}

		// Pick m distinct indices to blank.
		indices := rng.Perm(14)[:4]
		for _, idx := range indices {
			shards[idx] = nil
		}

		got, err := enc.Decode(shards)
		if err != nil {
			t.Fatalf("trial=%d decode: %v", trial, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("trial=%d payload mismatch", trial)
		}
	}
}

// TestTooManyLost verifies the typed error. Losing more than m shards must
// fail cleanly with ErrTooManyLost so the upper layer can account for it.
func TestTooManyLost(t *testing.T) {
	enc := mustDefaultEncoder(t)
	payload := makePayload(100, 4)

	shards, err := enc.Encode(payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Lose m+1 = 5 shards — one above recovery capacity.
	for i := range 5 {
		shards[i] = nil
	}

	if _, err := enc.Decode(shards); !errors.Is(err, ErrTooManyLost) {
		t.Fatalf("expected ErrTooManyLost, got %v", err)
	}
}

// TestShardSize is a pinning test on the capacity math. Transport code
// will compute per-frame slot counts from ShardSize, so a silent change in
// its behaviour would have downstream consequences.
func TestShardSize(t *testing.T) {
	enc := mustDefaultEncoder(t)
	cases := []struct {
		payload int
		want    int
	}{
		{0, 1},    // empty input still needs a 1-byte shard
		{1, 1},    // len prefix (4) + payload (1) = 5 → ceil(5/10) = 1
		{6, 1},    // 4+6 = 10 → 1
		{7, 2},    // 4+7 = 11 → 2
		{100, 11}, // 104 → ceil(104/10) = 11
		{297, 31}, // 301 → 31
	}

	for _, c := range cases {
		got := enc.ShardSize(c.payload)
		if got != c.want {
			t.Errorf("ShardSize(%d) = %d, want %d", c.payload, got, c.want)
		}
	}
}

// TestConstructorValidation covers the few corner cases that should fail
// fast rather than blow up inside reedsolomon.
func TestConstructorValidation(t *testing.T) {
	cases := []struct {
		n, m int
		ok   bool
	}{
		{10, 4, true},
		{0, 4, false},
		{10, 0, false},
		{200, 100, false}, // exceeds field size
	}
	for _, c := range cases {
		_, err := NewEncoder(c.n, c.m)
		if (err == nil) != c.ok {
			t.Errorf("NewEncoder(%d,%d): ok=%v err=%v", c.n, c.m, c.ok, err)
		}
	}
}

func mustDefaultEncoder(t *testing.T) *Encoder {
	t.Helper()
	const n = 10
	const m = 4
	e, err := NewEncoder(n, m)
	if err != nil {
		t.Fatalf("NewEncoder(%d,%d): %v", n, m, err)
	}
	return e
}

func makePayload(n int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // test-only
	out := make([]byte, n)
	_, _ = rng.Read(out)
	return out
}
