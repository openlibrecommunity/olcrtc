package multipath

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// setupBondPair builds a client Bond and a server Bond joined by n loopback
// mockTransport paths, connects both bonds, and registers cleanup to close
// them. It mirrors the real wiring contract: each mock's OnData is set to
// bond.PathOnData(index) before the path is handed to Bond.AddPath.
func setupBondPair(t *testing.T, n int) (client, server *Bond, clientPaths, serverPaths []*mockTransport) {
	t.Helper()
	return setupBondPairOpts(t, n, nil, nil)
}

// setupBondPairOpts is setupBondPair with per-side Bond options, so tests
// can shrink the recovery deadlines and timeouts to keep them fast.
func setupBondPairOpts(t *testing.T, n int, clientOpts, serverOpts []Option) (client, server *Bond, clientPaths, serverPaths []*mockTransport) {
	t.Helper()

	id := NewBondID()
	client = NewBond(id, RoleClient, clientOpts...)
	server = NewBond(id, RoleServer, serverOpts...)

	clientPaths = make([]*mockTransport, n)
	serverPaths = make([]*mockTransport, n)

	for i := 0; i < n; i++ {
		idx := uint16(i) //nolint:gosec // n is a small test path count
		ct := newMockTransport(fmt.Sprintf("client-path-%d", i))
		st := newMockTransport(fmt.Sprintf("server-path-%d", i))
		linkMockPair(ct, st)
		ct.setOnData(client.PathOnData(idx))
		st.setOnData(server.PathOnData(idx))
		client.AddPath(ct, idx)
		server.AddPath(st, idx)
		clientPaths[i] = ct
		serverPaths[i] = st
	}

	ctx := context.Background()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("client connect: %v", err)
	}
	if err := server.Connect(ctx); err != nil {
		t.Fatalf("server connect: %v", err)
	}

	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})

	return client, server, clientPaths, serverPaths
}

// collectInOrder wires bond's OnData to a buffered channel of copies, so
// tests can assert both content and arrival order without racing the
// delivery goroutines.
func collectInOrder(bond *Bond, capacity int) <-chan []byte {
	ch := make(chan []byte, capacity)
	bond.SetOnData(func(data []byte) {
		cp := make([]byte, len(data))
		copy(cp, data)
		ch <- cp
	})
	return ch
}

func expectMessages(t *testing.T, ch <-chan []byte, count int, timeout time.Duration) {
	t.Helper()
	for i := 0; i < count; i++ {
		want := fmt.Sprintf("msg-%06d", i)
		select {
		case got := <-ch:
			if string(got) != want {
				t.Fatalf("message %d out of order or corrupted: got %q want %q", i, got, want)
			}
		case <-time.After(timeout):
			t.Fatalf("timed out waiting for message %d/%d", i, count)
		}
	}
}

// TestBond_OrderedDeliveryAllPaths sends a large volume of messages across
// several healthy paths and checks the receiving Bond hands every one of
// them up, in order, with striping spread across all paths.
func TestBond_OrderedDeliveryAllPaths(t *testing.T) {
	const numPaths = 4
	const numMsgs = 2000

	client, server, _, _ := setupBondPair(t, numPaths)
	received := collectInOrder(server, numMsgs)

	for i := 0; i < numMsgs; i++ {
		msg := fmt.Sprintf("msg-%06d", i)
		if err := client.Send([]byte(msg)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	expectMessages(t, received, numMsgs, 10*time.Second)
}

// TestBond_PathDeathRequeue kills one of several paths partway through a
// transfer and verifies every message still arrives, in order, having been
// rerouted around the dead path.
func TestBond_PathDeathRequeue(t *testing.T) {
	const numPaths = 4
	const numMsgs = 3000
	const killAt = numMsgs / 3

	client, server, clientPaths, _ := setupBondPair(t, numPaths)
	received := collectInOrder(server, numMsgs)

	for i := 0; i < numMsgs; i++ {
		if i == killAt {
			clientPaths[1].kill("test-kill")
		}
		msg := fmt.Sprintf("msg-%06d", i)
		if err := client.Send([]byte(msg)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	expectMessages(t, received, numMsgs, 15*time.Second)

	if !client.CanSend() {
		t.Fatalf("expected client bond to still be able to send with paths remaining after a single path death")
	}
	if client.NumPaths() != numPaths {
		t.Fatalf("NumPaths should still count dead paths (they're registered, just not alive): got %d want %d",
			client.NumPaths(), numPaths)
	}
}

// TestBond_DedupDuplicateSeq drives Bond's frame handling directly (no
// mocks needed - this is pure reorder/dedup logic) to confirm that repeated
// and out-of-order sequence numbers never produce duplicate or malformed
// deliveries.
func TestBond_DedupDuplicateSeq(t *testing.T) {
	t.Run("duplicate in-order frame", func(t *testing.T) {
		server := NewBond(NewBondID(), RoleServer)
		t.Cleanup(func() { _ = server.Close() })

		var mu sync.Mutex
		var received [][]byte
		server.SetOnData(func(data []byte) {
			mu.Lock()
			received = append(received, append([]byte(nil), data...))
			mu.Unlock()
		})

		frame1 := encodeData(1, []byte("first"))
		frame2 := encodeData(2, []byte("second"))

		server.handleFrame(0, frame1)
		server.handleFrame(0, frame1) // exact duplicate, already-delivered seq
		server.handleFrame(0, frame2)
		server.handleFrame(0, frame1) // stale duplicate again

		mu.Lock()
		defer mu.Unlock()
		if len(received) != 2 {
			t.Fatalf("expected 2 delivered messages, got %d: %v", len(received), received)
		}
		if string(received[0]) != "first" || string(received[1]) != "second" {
			t.Fatalf("unexpected payloads: %q", received)
		}
	})

	t.Run("duplicate out-of-order frame", func(t *testing.T) {
		server := NewBond(NewBondID(), RoleServer)
		t.Cleanup(func() { _ = server.Close() })

		var mu sync.Mutex
		var received [][]byte
		server.SetOnData(func(data []byte) {
			mu.Lock()
			received = append(received, append([]byte(nil), data...))
			mu.Unlock()
		})

		// seq 2 arrives (and repeats) before seq 1 - it must be buffered,
		// not delivered, and the repeat must not double it up once seq 1
		// finally unblocks the reorder buffer.
		server.handleFrame(0, encodeData(2, []byte("second")))
		server.handleFrame(0, encodeData(2, []byte("second")))
		server.handleFrame(0, encodeData(1, []byte("first")))

		mu.Lock()
		defer mu.Unlock()
		if len(received) != 2 {
			t.Fatalf("expected 2 delivered messages, got %d: %v", len(received), received)
		}
		if string(received[0]) != "first" || string(received[1]) != "second" {
			t.Fatalf("unexpected payloads/order: %q", received)
		}
	})
}
