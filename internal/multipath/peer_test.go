package multipath

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/transport"
)

// mockPeerTransport models a peer-routing carrier that is SHARED across bonds
// (the Phase-5b server model): a single carrier addresses many remote peers by
// id. It implements transport.PeerTransport + transport.PeerControlPlane and
// demultiplexes purely by peerID. Every SendTo / ControlSendTo is recorded (so
// tests can assert reverse addressing) and, when a sink is registered for that
// peerID, also delivered (loopback) so a full path can be driven end to end.
type mockPeerTransport struct {
	*mockTransport

	mu       sync.Mutex
	dataSink map[string]func([]byte)
	ctlSink  map[string]func([]byte)
	sentData map[string][][]byte
	sentCtl  map[string][][]byte
	ctlDead  map[string]bool // peerID -> control plane reports not-ready
}

func newMockPeerTransport(name string) *mockPeerTransport {
	return &mockPeerTransport{
		mockTransport: newMockTransport(name),
		dataSink:      make(map[string]func([]byte)),
		ctlSink:       make(map[string]func([]byte)),
		sentData:      make(map[string][][]byte),
		sentCtl:       make(map[string][][]byte),
		ctlDead:       make(map[string]bool),
	}
}

func (m *mockPeerTransport) registerData(peerID string, sink func([]byte)) {
	m.mu.Lock()
	m.dataSink[peerID] = sink
	m.mu.Unlock()
}

func (m *mockPeerTransport) registerControl(peerID string, sink func([]byte)) {
	m.mu.Lock()
	m.ctlSink[peerID] = sink
	m.mu.Unlock()
}

// resetSent drops all recorded sends, so a test can ignore the PATH_HELLO frames
// emitted by AddPeerPath and observe only the sends it drives itself.
func (m *mockPeerTransport) resetSent() {
	m.mu.Lock()
	m.sentData = make(map[string][][]byte)
	m.sentCtl = make(map[string][][]byte)
	m.mu.Unlock()
}

func (m *mockPeerTransport) killControl(peerID string) {
	m.mu.Lock()
	m.ctlDead[peerID] = true
	m.mu.Unlock()
}

func (m *mockPeerTransport) dataFramesTo(peerID string) [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sentData[peerID]
}

func (m *mockPeerTransport) ctlFramesTo(peerID string) [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sentCtl[peerID]
}

func (m *mockPeerTransport) SendTo(peerID string, data []byte) error {
	if m.closed.get() || m.killed.get() {
		return errMockDead
	}
	cp := append([]byte(nil), data...)
	m.mu.Lock()
	m.sentData[peerID] = append(m.sentData[peerID], cp)
	sink := m.dataSink[peerID]
	m.mu.Unlock()
	if sink != nil {
		sink(cp)
	}
	return nil
}

func (m *mockPeerTransport) SupportsPeerRouting() bool { return true }

func (m *mockPeerTransport) ControlSendTo(peerID string, data []byte) error {
	if m.closed.get() || m.killed.get() {
		return errMockDead
	}
	cp := append([]byte(nil), data...)
	m.mu.Lock()
	m.sentCtl[peerID] = append(m.sentCtl[peerID], cp)
	sink := m.ctlSink[peerID]
	m.mu.Unlock()
	if sink != nil {
		sink(cp)
	}
	return nil
}

func (m *mockPeerTransport) SetControlOnPeerData(func(string, []byte)) {
	// Server-side tests feed inbound control directly via Bond.ControlIn, so the
	// carrier's shared callback is unused here.
}

func (m *mockPeerTransport) ControlPeerCanSend(peerID string) bool {
	if m.closed.get() || m.killed.get() {
		return false
	}
	m.mu.Lock()
	dead := m.ctlDead[peerID]
	m.mu.Unlock()
	return !dead
}

var (
	_ transport.PeerTransport    = (*mockPeerTransport)(nil)
	_ transport.PeerControlPlane = (*mockPeerTransport)(nil)
)

// hasFrameType reports whether any recorded frame in frames is of the given
// bond frame type.
func hasFrameType(frames [][]byte, ft frameType) bool {
	for _, f := range frames {
		if got, err := peekFrameType(f); err == nil && got == ft {
			return true
		}
	}
	return false
}

// TestBond_PeerRouting_SharedCarrier proves the Phase-5b invariant: two clients
// (two bond ids) sharing ONE peer-routing carrier map to two independent bonds,
// addressed on egress by distinct peerIDs, with no data or control cross-talk.
func TestBond_PeerRouting_SharedCarrier(t *testing.T) {
	ctx := context.Background()
	carrier := newMockPeerTransport("shared-carrier")
	if err := carrier.Connect(ctx); err != nil {
		t.Fatalf("carrier connect: %v", err)
	}

	bondA := NewBond(NewBondID(), RoleServer)
	bondB := NewBond(NewBondID(), RoleServer)
	if err := bondA.Connect(ctx); err != nil {
		t.Fatalf("bondA connect: %v", err)
	}
	if err := bondB.Connect(ctx); err != nil {
		t.Fatalf("bondB connect: %v", err)
	}
	t.Cleanup(func() { _ = bondA.Close(); _ = bondB.Close() })

	// Each bond owns one peer path over the shared carrier, addressed by its own
	// peerID - exactly what the server's addBondPeerPath does per client.
	bondA.AddPeerPath(carrier, "peerA", 0)
	bondB.AddPeerPath(carrier, "peerB", 0)

	// Wire the aggregate receive sinks (data + isolated control) as the server
	// would: data via Bond.SetOnData, control via AsCarrier(...).SetControlOnData.
	dataA := collectInOrder(bondA, 8)
	dataB := collectInOrder(bondB, 8)

	ctlA := make(chan []byte, 8)
	ctlB := make(chan []byte, 8)
	ccA, ok := AsCarrier(bondA).(transport.ControlPlane)
	if !ok {
		t.Fatalf("peer bondA should expose a control plane (per-peer control)")
	}
	ccB, ok := AsCarrier(bondB).(transport.ControlPlane)
	if !ok {
		t.Fatalf("peer bondB should expose a control plane (per-peer control)")
	}
	ccA.SetControlOnData(func(d []byte) { ctlA <- append([]byte(nil), d...) })
	ccB.SetControlOnData(func(d []byte) { ctlB <- append([]byte(nil), d...) })

	// Ignore the PATH_HELLO frames AddPeerPath emitted; observe only what the
	// bonds send from here on.
	carrier.resetSent()

	// --- egress data addressing + isolation -------------------------------
	if err := bondA.Send([]byte("payload-A")); err != nil {
		t.Fatalf("bondA.Send: %v", err)
	}
	if err := bondB.Send([]byte("payload-B")); err != nil {
		t.Fatalf("bondB.Send: %v", err)
	}
	if !hasFrameType(carrier.dataFramesTo("peerA"), frameTypeData) {
		t.Fatalf("bondA.Send did not address a DATA frame to peerA")
	}
	if !hasFrameType(carrier.dataFramesTo("peerB"), frameTypeData) {
		t.Fatalf("bondB.Send did not address a DATA frame to peerB")
	}
	// bondA's data must never be addressed to peerB and vice versa.
	if hasFrameType(carrier.dataFramesTo("peerB"), frameTypeData) && len(carrier.dataFramesTo("peerB")) != 1 {
		// peerB legitimately has exactly bondB's one DATA frame; ensure bondA's
		// payload did not also land there.
		for _, f := range carrier.dataFramesTo("peerB") {
			if _, payload, err := decodeData(f); err == nil && string(payload) == "payload-A" {
				t.Fatalf("bondA payload leaked to peerB")
			}
		}
	}

	// --- ingress data isolation (server demux feeds the right path) --------
	bondA.PathOnData(0)(encodeData(1, []byte("in-A")))
	select {
	case got := <-dataA:
		if string(got) != "in-A" {
			t.Fatalf("bondA ingress payload mismatch: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bondA did not deliver its ingress data frame")
	}
	select {
	case leaked := <-dataB:
		t.Fatalf("bondA ingress data leaked into bondB: %q", leaked)
	case <-time.After(100 * time.Millisecond):
	}

	// --- egress control addressing ----------------------------------------
	if err := ccA.ControlSend([]byte("ctl-A")); err != nil {
		t.Fatalf("bondA ControlSend: %v", err)
	}
	if len(carrier.ctlFramesTo("peerA")) == 0 {
		t.Fatalf("bondA control was not addressed to peerA")
	}
	if len(carrier.ctlFramesTo("peerB")) != 0 {
		t.Fatalf("bondA control leaked to peerB")
	}

	// --- ingress control isolation (Bond.ControlIn) -----------------------
	bondA.ControlIn([]byte("ctl-in-A"))
	select {
	case got := <-ctlA:
		if string(got) != "ctl-in-A" {
			t.Fatalf("bondA control ingress mismatch: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bondA did not deliver its ingress control frame")
	}
	select {
	case leaked := <-ctlB:
		t.Fatalf("bondA control ingress leaked into bondB: %q", leaked)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestBond_PeerRouting_ControlFailover kills the sticky control peer path and
// checks the next ControlSend fails over to the surviving peer path, addressing
// its distinct peerID.
func TestBond_PeerRouting_ControlFailover(t *testing.T) {
	ctx := context.Background()
	carrier := newMockPeerTransport("carrier")
	if err := carrier.Connect(ctx); err != nil {
		t.Fatalf("carrier connect: %v", err)
	}

	bond := NewBond(NewBondID(), RoleServer)
	if err := bond.Connect(ctx); err != nil {
		t.Fatalf("bond connect: %v", err)
	}
	t.Cleanup(func() { _ = bond.Close() })

	// Two peer paths for the same client over two carriers is the real shape;
	// here one carrier with two peerIDs suffices to exercise control failover.
	bond.AddPeerPath(carrier, "p0", 0)
	bond.AddPeerPath(carrier, "p1", 1)

	cc, ok := AsCarrier(bond).(transport.ControlPlane)
	if !ok {
		t.Fatalf("bond should expose a control plane")
	}
	carrier.resetSent()

	// First send establishes the sticky selection (lowest alive index = 0 -> p0).
	if err := cc.ControlSend([]byte("pre")); err != nil {
		t.Fatalf("ControlSend pre: %v", err)
	}
	if len(carrier.ctlFramesTo("p0")) == 0 {
		t.Fatalf("first control send should have addressed p0")
	}

	// Kill p0's control plane; the next send must fail over to p1.
	carrier.killControl("p0")
	if err := cc.ControlSend([]byte("post")); err != nil {
		t.Fatalf("ControlSend post (failover): %v", err)
	}
	if len(carrier.ctlFramesTo("p1")) == 0 {
		t.Fatalf("control did not fail over to p1 after p0's control plane died")
	}
}
