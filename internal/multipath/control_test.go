package multipath

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/transport"
)

// controlMock is a mockTransport that additionally exposes an isolated
// control plane (transport.ControlPlane), modelling a vp8channel-style carrier
// with a second, priority KCP session. The control plane is a synchronous
// loopback to the paired controlMock's control sink - control traffic is
// low-volume, so tests do not need the async delivery loop the data plane uses.
type controlMock struct {
	*mockTransport
	ctlPeer *controlMock

	ctlMu     sync.Mutex
	ctlOnData func([]byte)
}

func newControlMock(name string) *controlMock {
	return &controlMock{mockTransport: newMockTransport(name)}
}

// linkControlMockPair links both the data plane (via linkMockPair) and the
// control plane of two controlMocks.
func linkControlMockPair(a, b *controlMock) {
	linkMockPair(a.mockTransport, b.mockTransport)
	a.ctlPeer = b
	b.ctlPeer = a
}

func (c *controlMock) ControlSend(data []byte) error {
	if c.closed.get() || c.killed.get() {
		return errMockDead
	}
	if !c.connected.get() {
		return errMockNotConnected
	}
	peer := c.ctlPeer
	if peer == nil {
		return nil
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	peer.ctlMu.Lock()
	cb := peer.ctlOnData
	peer.ctlMu.Unlock()
	if cb != nil {
		cb(cp)
	}
	return nil
}

func (c *controlMock) SetControlOnData(cb func([]byte)) {
	c.ctlMu.Lock()
	c.ctlOnData = cb
	c.ctlMu.Unlock()
}

func (c *controlMock) ControlCanSend() bool {
	return c.connected.get() && !c.closed.get() && !c.killed.get()
}

var (
	_ transport.Transport    = (*controlMock)(nil)
	_ transport.ControlPlane = (*controlMock)(nil)
)

// setupControlBondPair builds a client and server bond joined by n loopback
// control-capable paths and returns both bonds plus the client-side mock paths
// (so tests can kill them). Both bonds are connected and cleaned up.
func setupControlBondPair(t *testing.T, n int) (client, server *Bond, clientPaths []*controlMock) {
	t.Helper()

	id := NewBondID()
	client = NewBond(id, RoleClient)
	server = NewBond(id, RoleServer)

	clientPaths = make([]*controlMock, n)
	for i := 0; i < n; i++ {
		idx := uint16(i) //nolint:gosec // small test path count
		ct := newControlMock(fmt.Sprintf("client-ctl-%d", i))
		st := newControlMock(fmt.Sprintf("server-ctl-%d", i))
		linkControlMockPair(ct, st)
		ct.setOnData(client.PathOnData(idx))
		st.setOnData(server.PathOnData(idx))
		client.AddPath(ct, idx)
		server.AddPath(st, idx)
		clientPaths[i] = ct
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
	return client, server, clientPaths
}

// TestAsCarrier_ControlCapability checks the central Phase-5a invariant: a bond
// whose paths all carry a control plane is handed out as a value that satisfies
// transport.ControlPlane, while a bond of plain paths is NOT - so
// muxconn.NewControl only builds a control session when one can actually exist.
func TestAsCarrier_ControlCapability(t *testing.T) {
	t.Run("all control-capable paths -> ControlPlane", func(t *testing.T) {
		b := NewBond(NewBondID(), RoleClient)
		b.AddPath(newControlMock("c0"), 0)
		b.AddPath(newControlMock("c1"), 1)
		carrier := AsCarrier(b)
		if _, ok := carrier.(transport.ControlPlane); !ok {
			t.Fatalf("all-control bond should be exposed as transport.ControlPlane")
		}
	})

	t.Run("plain paths -> NOT ControlPlane", func(t *testing.T) {
		b := NewBond(NewBondID(), RoleClient)
		b.AddPath(newMockTransport("p0"), 0)
		b.AddPath(newMockTransport("p1"), 1)
		carrier := AsCarrier(b)
		if _, ok := carrier.(transport.ControlPlane); ok {
			t.Fatalf("plain bond must NOT be exposed as transport.ControlPlane (would break inline control)")
		}
	})

	t.Run("mixed paths -> NOT ControlPlane (inline fallback)", func(t *testing.T) {
		b := NewBond(NewBondID(), RoleClient)
		b.AddPath(newControlMock("c0"), 0)
		b.AddPath(newMockTransport("p1"), 1)
		carrier := AsCarrier(b)
		if _, ok := carrier.(transport.ControlPlane); ok {
			t.Fatalf("mixed bond must fall back to inline (bare *Bond), not claim a control plane")
		}
	})
}

// TestControlProxy_SendReceive verifies a control frame sent through the bond's
// control plane reaches the peer bond's control sink, and that ControlCanSend
// tracks path liveness.
func TestControlProxy_SendReceive(t *testing.T) {
	client, server, _ := setupControlBondPair(t, 3)

	cc, ok := AsCarrier(client).(transport.ControlPlane)
	if !ok {
		t.Fatalf("client bond should expose a control plane")
	}
	sc, ok := AsCarrier(server).(transport.ControlPlane)
	if !ok {
		t.Fatalf("server bond should expose a control plane")
	}

	if !cc.ControlCanSend() {
		t.Fatalf("ControlCanSend should be true with alive paths")
	}

	got := make(chan []byte, 8)
	sc.SetControlOnData(func(d []byte) {
		cp := make([]byte, len(d))
		copy(cp, d)
		got <- cp
	})

	if err := cc.ControlSend([]byte("hello-control")); err != nil {
		t.Fatalf("ControlSend: %v", err)
	}
	select {
	case d := <-got:
		if string(d) != "hello-control" {
			t.Fatalf("control payload mismatch: got %q", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("control frame not received")
	}
}

// TestControlProxy_ReceiveFromAnyPath checks that a control frame arriving on a
// path other than path 0 is still delivered up: the receive sink is aggregated
// across every control-capable path.
func TestControlProxy_ReceiveFromAnyPath(t *testing.T) {
	client, server, _ := setupControlBondPair(t, 3)

	got := make(chan []byte, 4)
	server.setControlOnData(func(d []byte) {
		cp := make([]byte, len(d))
		copy(cp, d)
		got <- cp
	})

	// Drive the client's control send directly at path index 2 to prove the
	// server aggregates receipt across all paths, not just the sticky one.
	client.mu.RLock()
	p2 := client.paths[2]
	client.mu.RUnlock()
	if err := p2.control.ControlSend([]byte("from-path-2")); err != nil {
		t.Fatalf("ControlSend on path 2: %v", err)
	}
	select {
	case d := <-got:
		if string(d) != "from-path-2" {
			t.Fatalf("payload mismatch: got %q", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("control frame from path 2 not received")
	}
}

// TestControlProxy_Failover kills the sticky control path mid-stream and checks
// the next ControlSend fails over to a surviving path and still delivers.
func TestControlProxy_Failover(t *testing.T) {
	client, server, clientPaths := setupControlBondPair(t, 3)

	cc := &controlBond{Bond: client}

	got := make(chan []byte, 8)
	server.setControlOnData(func(d []byte) {
		cp := make([]byte, len(d))
		copy(cp, d)
		got <- cp
	})

	// First send establishes the sticky selection (lowest alive index = 0).
	if err := cc.ControlSend([]byte("pre-kill")); err != nil {
		t.Fatalf("ControlSend pre-kill: %v", err)
	}
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("pre-kill frame not received")
	}

	client.mu.RLock()
	sel := client.controlSelected
	client.mu.RUnlock()
	// Kill whichever path the control stream selected.
	clientPaths[sel].kill("test-failover")

	// Give the ended callback a moment to mark the path dead.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		client.mu.RLock()
		alive := client.paths[sel].isAlive()
		client.mu.RUnlock()
		if !alive {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := cc.ControlSend([]byte("post-kill")); err != nil {
		t.Fatalf("ControlSend post-kill (expected failover): %v", err)
	}
	select {
	case d := <-got:
		if string(d) != "post-kill" {
			t.Fatalf("payload mismatch after failover: got %q", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post-kill frame not received (failover failed)")
	}

	// The new sticky selection must be a different, still-alive path.
	client.mu.RLock()
	newSel := client.controlSelected
	client.mu.RUnlock()
	if newSel == sel {
		t.Fatalf("control selection did not fail over off dead path %d", sel)
	}
}

// TestControlProxy_CanSendFalseWhenAllDead verifies ControlCanSend flips to
// false once every control-capable path is dead.
func TestControlProxy_CanSendFalseWhenAllDead(t *testing.T) {
	client, _, clientPaths := setupControlBondPair(t, 2)
	cc := &controlBond{Bond: client}

	if !cc.ControlCanSend() {
		t.Fatalf("expected ControlCanSend true initially")
	}
	for _, p := range clientPaths {
		p.kill("test")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && cc.ControlCanSend() {
		time.Sleep(5 * time.Millisecond)
	}
	if cc.ControlCanSend() {
		t.Fatalf("expected ControlCanSend false after all paths killed")
	}
	if err := cc.ControlSend([]byte("x")); err != ErrNoControlPath {
		t.Fatalf("expected ErrNoControlPath, got %v", err)
	}
}

// TestControlProxy_SetOnDataBeforePathAdded covers the ordering where the
// control receive callback is registered before a control-capable path joins:
// AddPath must retro-wire the pending callback onto the late path.
func TestControlProxy_SetOnDataBeforePathAdded(t *testing.T) {
	server := NewBond(NewBondID(), RoleServer)
	got := make(chan []byte, 4)
	server.setControlOnData(func(d []byte) {
		cp := make([]byte, len(d))
		copy(cp, d)
		got <- cp
	})

	// Path added AFTER the callback was set.
	st := newControlMock("late-server")
	ct := newControlMock("late-client")
	linkControlMockPair(ct, st)
	st.setOnData(server.PathOnData(0))
	_ = st.Connect(context.Background())
	_ = ct.Connect(context.Background())
	server.AddPath(st, 0)
	t.Cleanup(func() { _ = server.Close() })

	if err := ct.ControlSend([]byte("late")); err != nil {
		t.Fatalf("ControlSend: %v", err)
	}
	select {
	case d := <-got:
		if string(d) != "late" {
			t.Fatalf("payload mismatch: got %q", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback set before path add was not wired to late path")
	}
}
