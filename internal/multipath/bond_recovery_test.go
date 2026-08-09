package multipath

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// waitForCond polls cond until it returns true or the budget is spent.
//
// ai-generated: helper for the recovery tests below.
func waitForCond(t *testing.T, budget time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

// TestBond_SilentPathDeathRequeuesAfterDeadline kills one of two paths
// silently (blackhole: delivery stops, no ended callback fires - the carrier
// whose liveness never announced the death) and verifies that after the
// resend deadline the sender's reaper re-queues the stranded frames onto the
// surviving path and the receiver delivers the whole stream in order. Before
// the reaper this left pending frames unacked forever and the receiver stuck
// on a hole.
//
// ai-generated: this test for the time-based reaper.
func TestBond_SilentPathDeathRequeuesAfterDeadline(t *testing.T) {
	const numPaths = 2
	const numMsgs = 2000
	const killAt = numMsgs / 3

	deadline := 50 * time.Millisecond
	client, server, clientPaths, _ := setupBondPairOpts(t, numPaths,
		[]Option{WithResendDeadline(deadline)}, nil)
	received := collectInOrder(server, numMsgs)

	for i := 0; i < numMsgs; i++ {
		if i == killAt {
			clientPaths[1].blackhole()
		}
		msg := fmt.Sprintf("msg-%06d", i)
		if err := client.Send([]byte(msg)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	expectMessages(t, received, numMsgs, 20*time.Second)

	if clientPaths[1].CanSend() {
		t.Fatal("blackholed path must not report sendable anymore")
	}
	if !client.CanSend() {
		t.Fatal("client bond should still be able to send via the surviving path")
	}

	// The sender side must have drained its pending buffer: everything was
	// eventually acked, so nothing is left stranded on the dead path.
	waitForCond(t, 3*time.Second, func() bool {
		client.sendMu.Lock()
		defer client.sendMu.Unlock()
		return len(client.pending) == 0
	}, "pending buffer to drain after reaper requeue")
}

// TestBond_PermanentHoleEvictedWithTail bounds the reorder buffer and
// verifies the drop-hole policy: a sequence gap that ages out beyond the
// hole timeout with a buffered tail is presumed lost, the hole is dropped,
// and the buffered tail is delivered in order - so a permanent hole cannot
// stall the stream forever nor accumulate unbounded memory.
//
// ai-generated: this test for the bounded reorder window and hole eviction.
func TestBond_PermanentHoleEvictedWithTail(t *testing.T) {
	server := NewBond(NewBondID(), RoleServer, WithHoleTimeout(100*time.Millisecond))
	t.Cleanup(func() { _ = server.Close() })

	var mu sync.Mutex
	var received []string
	server.SetOnData(func(data []byte) {
		mu.Lock()
		received = append(received, string(data))
		mu.Unlock()
	})

	// A frame farther ahead than the reorder window must be dropped, not
	// buffered: buffering it could only grow memory behind an unrecoverable
	// gap.
	farAhead := uint64(maxReorderWindow + 2)
	server.handleFrame(0, encodeData(farAhead, []byte("far-ahead")))

	// A hole at seq 1 with a buffered tail: seq 2 and 3 arrive, seq 1 never
	// does. After the hole timeout the receiver must deliver the tail.
	server.handleFrame(0, encodeData(2, []byte("second")))
	server.handleFrame(0, encodeData(3, []byte("third")))

	waitForCond(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) == 2
	}, "buffered tail delivery after hole eviction")

	mu.Lock()
	entries := append([]string(nil), received...)
	mu.Unlock()
	if entries[0] != "second" || entries[1] != "third" {
		t.Fatalf("tail delivered out of order: %q", entries)
	}

	server.recvMu.Lock()
	buffered := len(server.reorderBuf)
	expected := server.expectedSeq
	server.recvMu.Unlock()
	if buffered != 0 {
		t.Fatalf("reorder buffer not empty after eviction: %d entries", buffered)
	}
	if expected != 4 {
		t.Fatalf("expectedSeq after eviction = %d, want 4", expected)
	}

	// Re-sends of the presumed-lost hole (seq 1) must be dropped as stale,
	// not delivered twice.
	server.handleFrame(0, encodeData(1, []byte("hole-late")))
	mu.Lock()
	late := len(received)
	mu.Unlock()
	if late != 2 {
		t.Fatalf("stale hole re-send delivered (count=%d), want 2", late)
	}
}

// TestBond_ResetPeerRestartsSeqSpaces feeds a bond pair, resets both sides,
// and verifies the seq spaces restart cleanly: pending and the reorder
// buffer are empty, the next send starts at seq 1, and the receiver accepts
// it instead of dropping it as stale.
//
// ai-generated: this test for the ResetPeer seq-space restart.
func TestBond_ResetPeerRestartsSeqSpaces(t *testing.T) {
	const numPaths = 2
	const perSession = 100

	client, server, _, _ := setupBondPair(t, numPaths)
	received := collectInOrder(server, 2*perSession)

	for i := 0; i < perSession; i++ {
		if err := client.Send([]byte(fmt.Sprintf("msg-%06d", i))); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	expectMessages(t, received, perSession, 10*time.Second)

	// Tear the logical session down on both ends, as a session rebuild does.
	client.ResetPeer()
	server.ResetPeer()

	client.sendMu.Lock()
	nPending := len(client.pending)
	nextSeq := client.sendSeq.Load()
	client.sendMu.Unlock()
	if nPending != 0 {
		t.Fatalf("pending not cleared by ResetPeer: %d frames", nPending)
	}
	if nextSeq != 0 {
		t.Fatalf("sendSeq after ResetPeer = %d, want 0 (next send gets seq 1)", nextSeq)
	}
	server.recvMu.Lock()
	nBuffered := len(server.reorderBuf)
	expected := server.expectedSeq
	server.recvMu.Unlock()
	if nBuffered != 0 {
		t.Fatalf("reorder buffer not cleared by ResetPeer: %d frames", nBuffered)
	}
	if expected != 1 {
		t.Fatalf("expectedSeq after ResetPeer = %d, want 1", expected)
	}

	// The new session continues on a clean sequence space: frames restart at
	// seq 1 and must be accepted, not dropped as stale replay.
	for i := 0; i < perSession; i++ {
		msg := fmt.Sprintf("msg-%06d", perSession+i)
		if err := client.Send([]byte(msg)); err != nil {
			t.Fatalf("post-reset send %d: %v", i, err)
		}
	}
	for i := 0; i < perSession; i++ {
		want := fmt.Sprintf("msg-%06d", perSession+i)
		select {
		case got := <-received:
			if string(got) != want {
				t.Fatalf("post-reset message %d corrupted: got %q want %q", i, got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for post-reset message %s", want)
		}
	}
}
