package jitsi

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A session whose sendLoop never runs: whatever is enqueued stays enqueued,
// which is the saturated-link case olcbox #15 reproduced with a Speedtest
// upload - control pongs behind 80 MB of queue, then a full queue closing
// smux outright.
func newQueuedSession(t *testing.T) *Session {
	t.Helper()
	s := newSilentSession(t)
	s.bridgeReady.Store(true)
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSendWaitsForRoomInsteadOfFailingWhenTheQueueIsFull(t *testing.T) {
	s := newQueuedSession(t)
	for i := range defaultSendQueueSize {
		if err := s.Send([]byte("x")); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	var done atomic.Bool
	go func() {
		_ = s.Send([]byte("one more"))
		done.Store(true)
	}()
	time.Sleep(100 * time.Millisecond)
	if done.Load() {
		t.Fatal("Send returned on a full queue; it has to wait for room")
	}
	<-s.sendQueue // the link drained one frame
	waitFor(t, "Send did not resume after the queue drained", done.Load)
}

func TestAWaitingSendReturnsWhenTheSessionCloses(t *testing.T) {
	s := newQueuedSession(t)
	for i := range defaultSendQueueSize {
		if err := s.Send([]byte("x")); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	errCh := make(chan error, 1)
	go func() { errCh <- s.Send([]byte("blocked")) }()
	time.Sleep(50 * time.Millisecond)
	_ = s.Close()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrSessionClosed) {
			t.Fatalf("err = %v, want ErrSessionClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not release the waiting send")
	}
}

func TestAFullQueueForOnePeerDoesNotHoldAnother(t *testing.T) {
	s := newQueuedSession(t)
	for i := range defaultSendQueueSize {
		if err := s.SendTo("peer-a", []byte("x")); err != nil {
			t.Fatalf("peer-a send %d: %v", i, err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- s.SendTo("peer-b", []byte("y")) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("peer-b: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("peer-b waited on peer-a's queue")
	}
}

func TestDrainingMakesRoomForAWaitingPeerSend(t *testing.T) {
	s := newQueuedSession(t)
	for i := range defaultSendQueueSize {
		if err := s.SendTo("peer-a", []byte("x")); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	var done atomic.Bool
	go func() {
		_ = s.SendTo("peer-a", []byte("late"))
		done.Store(true)
	}()
	time.Sleep(50 * time.Millisecond)
	if done.Load() {
		t.Fatal("SendTo returned on a full peer queue")
	}
	// A reconnect drains the queues in place; the waiting sender gets its
	// slot rather than a channel nobody reads any more. Its frame may be
	// swept up by the same drain - it predates the reconnect too - so the
	// only promise is that the sender is free and the queue is near empty.
	s.drainSendQueue()
	waitFor(t, "SendTo did not resume after the drain", done.Load)
	if got := len(s.peerQueues["peer-a"].ch); got > 1 {
		t.Fatalf("peer-a queue holds %d frames after the drain", got)
	}
}

func TestIdleUnreferencedPeerQueuesAreDropped(t *testing.T) {
	s := newQueuedSession(t)
	if err := s.SendTo("peer-a", []byte("x")); err != nil {
		t.Fatal(err)
	}
	<-s.peerQueues["peer-a"].ch
	s.peerQueueMu.Lock()
	s.peerQueues["peer-a"].lastUsed = time.Now().Add(-2 * peerQueueIdle)
	s.peerQueueMu.Unlock()
	s.reapPeerQueues()
	if _, still := s.peerQueues["peer-a"]; still {
		t.Fatal("an idle, empty, unreferenced peer queue survived the reap")
	}
	// A referenced one does not go, however old.
	pq := s.peerQueueFor("peer-b")
	s.peerQueueMu.Lock()
	pq.lastUsed = time.Now().Add(-2 * peerQueueIdle)
	s.peerQueueMu.Unlock()
	s.reapPeerQueues()
	if _, kept := s.peerQueues["peer-b"]; !kept {
		t.Fatal("a peer queue with a sender still holding it was dropped")
	}
	s.releasePeerQueue(pq)
}
