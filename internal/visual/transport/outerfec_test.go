package transport

import (
	"bytes"
	"testing"
	"time"
)

// TestOuterFECHappyPath feeds a complete group through encoder→decoder and
// verifies every payload is delivered in order.
func TestOuterFECHappyPath(t *testing.T) {
	const n, m = 4, 2
	enc := NewOuterFECEncoder(n, m)
	dec := NewOuterFECDecoder(n, m)

	payloads := [][]byte{
		[]byte("payload-zero"),
		[]byte("payload-one"),
		[]byte("payload-two"),
		[]byte("payload-three"),
	}

	var allWrapped [][]byte
	for _, p := range payloads {
		out, err := enc.Add(p)
		if err != nil {
			t.Fatalf("encoder.Add: %v", err)
		}
		allWrapped = append(allWrapped, out...)
	}

	// allWrapped = [d0, d1, d2, d3, par0, par1]
	var delivered [][]byte
	for _, w := range allWrapped {
		results, err := dec.Accept(w)
		if err != nil {
			t.Fatalf("decoder.Accept: %v", err)
		}
		delivered = append(delivered, results...)
	}

	if len(delivered) != n {
		t.Fatalf("delivered %d payloads, want %d", len(delivered), n)
	}
	for i, want := range payloads {
		if !bytes.Equal(delivered[i], want) {
			t.Errorf("payload[%d] = %q, want %q", i, delivered[i], want)
		}
	}

	stats := dec.Stats()
	if stats.GroupsCompleted != 1 {
		t.Errorf("GroupsCompleted = %d, want 1", stats.GroupsCompleted)
	}
	if stats.FramesRecovered != 0 {
		t.Errorf("FramesRecovered = %d, want 0 (no loss)", stats.FramesRecovered)
	}
}

// TestOuterFECRecoverMissing drops one data shard and verifies RS recovery
// delivers it via the parity shards.
func TestOuterFECRecoverMissing(t *testing.T) {
	const n, m = 4, 2
	enc := NewOuterFECEncoder(n, m)
	dec := NewOuterFECDecoder(n, m)

	payloads := [][]byte{
		[]byte("recover-zero"),
		[]byte("recover-one-lost"),
		[]byte("recover-two"),
		[]byte("recover-three"),
	}

	var allWrapped [][]byte
	for _, p := range payloads {
		out, err := enc.Add(p)
		if err != nil {
			t.Fatalf("encoder.Add: %v", err)
		}
		allWrapped = append(allWrapped, out...)
	}

	// Drop data shard at index 1 (payload[1]).
	var delivered [][]byte
	for i, w := range allWrapped {
		if i == 1 {
			continue // simulate packet loss
		}
		results, err := dec.Accept(w)
		if err != nil {
			t.Fatalf("decoder.Accept[%d]: %v", i, err)
		}
		delivered = append(delivered, results...)
	}

	if len(delivered) != n {
		t.Fatalf("delivered %d payloads, want %d (including recovered)", len(delivered), n)
	}

	stats := dec.Stats()
	if stats.FramesRecovered != 1 {
		t.Errorf("FramesRecovered = %d, want 1", stats.FramesRecovered)
	}
	if stats.GroupsCompleted != 1 {
		t.Errorf("GroupsCompleted = %d, want 1", stats.GroupsCompleted)
	}
}

// TestOuterFECTooManyLost drops more shards than parity can cover and verifies
// no recovery is attempted.
func TestOuterFECTooManyLost(t *testing.T) {
	const n, m = 4, 2
	enc := NewOuterFECEncoder(n, m)
	dec := NewOuterFECDecoder(n, m)

	payloads := [][]byte{
		[]byte("toomany-0"),
		[]byte("toomany-1"),
		[]byte("toomany-2"),
		[]byte("toomany-3"),
	}

	var allWrapped [][]byte
	for _, p := range payloads {
		out, err := enc.Add(p)
		if err != nil {
			t.Fatalf("encoder.Add: %v", err)
		}
		allWrapped = append(allWrapped, out...)
	}

	// Drop data shards 0, 1, 2 — only 3+2=5 shards, but we lose 3 data so
	// we only have 1 data + 2 parity = 3 shards < n=4 → unrecoverable.
	var delivered [][]byte
	for i, w := range allWrapped {
		if i < 3 {
			continue // drop first three data shards
		}
		results, err := dec.Accept(w)
		if err != nil {
			t.Fatalf("decoder.Accept[%d]: %v", i, err)
		}
		delivered = append(delivered, results...)
	}

	if len(delivered) != 1 {
		// Only payload[3] arrives directly; the others are unrecoverable.
		t.Fatalf("delivered %d payloads, want 1 (unrecoverable group)", len(delivered))
	}

	stats := dec.Stats()
	if stats.FramesRecovered != 0 {
		t.Errorf("FramesRecovered = %d, want 0 (unrecoverable)", stats.FramesRecovered)
	}
}

// TestOuterFECSweepExpired verifies that stale groups are cleaned up, and that
// undelivered data payloads are counted as lost.
func TestOuterFECSweepExpired(t *testing.T) {
	const n, m = 4, 2
	now := time.Now()
	dec := NewOuterFECDecoder(n, m)
	dec.now = func() time.Time { return now }

	enc := NewOuterFECEncoder(n, m)
	payloads := [][]byte{
		[]byte("sweep-0"),
		[]byte("sweep-1"),
		[]byte("sweep-2"),
		[]byte("sweep-3"),
	}

	var allWrapped [][]byte
	for _, p := range payloads {
		out, err := enc.Add(p)
		if err != nil {
			t.Fatalf("encoder.Add: %v", err)
		}
		allWrapped = append(allWrapped, out...)
	}

	// Deliver only the first two data shards; leave the rest unseen.
	for i := range 2 {
		if _, err := dec.Accept(allWrapped[i]); err != nil {
			t.Fatalf("decoder.Accept[%d]: %v", i, err)
		}
	}

	// Advance clock past the timeout.
	dec.now = func() time.Time { return now.Add(31 * time.Second) }

	removed := dec.SweepExpired()
	if removed != 1 {
		t.Errorf("SweepExpired removed %d groups, want 1", removed)
	}

	stats := dec.Stats()
	if stats.FramesLost != 2 { // payload[2] and payload[3] never delivered
		t.Errorf("FramesLost = %d, want 2", stats.FramesLost)
	}
	if stats.GroupsAbandoned != 1 {
		t.Errorf("GroupsAbandoned = %d, want 1", stats.GroupsAbandoned)
	}
}

// TestOuterFECMultiGroup exercises two consecutive groups to verify groupID
// wrapping and that the decoder keeps groups isolated.
func TestOuterFECMultiGroup(t *testing.T) {
	const n, m = 3, 1
	enc := NewOuterFECEncoder(n, m)
	dec := NewOuterFECDecoder(n, m)

	makePayloads := func(prefix string) [][]byte {
		return [][]byte{
			[]byte(prefix + "-0"),
			[]byte(prefix + "-1"),
			[]byte(prefix + "-2"),
		}
	}

	var totalDelivered int
	for gi, group := range [][]byte{nil, nil} {
		_ = group
		payloads := makePayloads([]string{"alpha", "beta"}[gi])
		for _, p := range payloads {
			out, err := enc.Add(p)
			if err != nil {
				t.Fatalf("group %d encoder.Add: %v", gi, err)
			}
			for _, w := range out {
				results, err := dec.Accept(w)
				if err != nil {
					t.Fatalf("group %d decoder.Accept: %v", gi, err)
				}
				totalDelivered += len(results)
			}
		}
	}

	if totalDelivered != 2*n {
		t.Errorf("totalDelivered = %d, want %d", totalDelivered, 2*n)
	}

	stats := dec.Stats()
	if stats.GroupsCompleted != 2 {
		t.Errorf("GroupsCompleted = %d, want 2", stats.GroupsCompleted)
	}
}

// TestOuterFECRoundTripVariablePayloadSize checks that payloads of different
// lengths within the same group all survive the encode→decode cycle.
func TestOuterFECRoundTripVariablePayloadSize(t *testing.T) {
	const n, m = 4, 2
	enc := NewOuterFECEncoder(n, m)
	dec := NewOuterFECDecoder(n, m)

	payloads := [][]byte{
		bytes.Repeat([]byte{0xAA}, 1),
		bytes.Repeat([]byte{0xBB}, 100),
		bytes.Repeat([]byte{0xCC}, 500),
		bytes.Repeat([]byte{0xDD}, 1000),
	}

	var allWrapped [][]byte
	for _, p := range payloads {
		out, err := enc.Add(p)
		if err != nil {
			t.Fatalf("encoder.Add (len=%d): %v", len(p), err)
		}
		allWrapped = append(allWrapped, out...)
	}

	// Drop the shortest (index 0); recover it via parity.
	var delivered [][]byte
	for i, w := range allWrapped {
		if i == 0 {
			continue
		}
		results, err := dec.Accept(w)
		if err != nil {
			t.Fatalf("decoder.Accept[%d]: %v", i, err)
		}
		delivered = append(delivered, results...)
	}

	if len(delivered) != n {
		t.Fatalf("delivered %d payloads, want %d", len(delivered), n)
	}

	// Payload[0] is recovered; find it among delivered (order may differ).
	found := false
	for _, d := range delivered {
		if bytes.Equal(d, payloads[0]) {
			found = true
			break
		}
	}
	if !found {
		t.Error("recovered payload[0] not found in delivered results")
	}
}
