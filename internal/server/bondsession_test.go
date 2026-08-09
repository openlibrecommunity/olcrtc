package server

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/control"
	cryptopkg "github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/framing"
	"github.com/openlibrecommunity/olcrtc/internal/handshake"
	"github.com/openlibrecommunity/olcrtc/internal/multipath"
	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/xtaci/smux"
)

// ---------------------------------------------------------------------------
// In-memory carrier pair used to drive bond sessions end to end.
// ---------------------------------------------------------------------------

// memLink is an in-memory message-oriented transport.Transport pair: Send
// hands one message to the peer side's OnData via a buffered channel drained
// by a pump goroutine, preserving order and message boundaries like a real
// carrier. It has no isolated control plane, so a bond over it runs the
// handshake inline on the data session (like a datachannel carrier).
type memLink struct {
	ch     chan []byte
	closed chan struct{}
	peer   *memLink
	once   sync.Once
	mu     sync.Mutex
	onData func([]byte)
}

func newMemLinkPair() (*memLink, *memLink) {
	a := &memLink{ch: make(chan []byte, 512), closed: make(chan struct{})}
	b := &memLink{ch: make(chan []byte, 512), closed: make(chan struct{})}
	a.peer = b
	b.peer = a
	return a, b
}

func (m *memLink) setOnData(cb func([]byte)) {
	m.mu.Lock()
	m.onData = cb
	m.mu.Unlock()
}

func (m *memLink) start() {
	m.once.Do(func() { go m.pump() })
}

func (m *memLink) pump() {
	for {
		select {
		case d := <-m.ch:
			m.mu.Lock()
			cb := m.onData
			m.mu.Unlock()
			if cb != nil {
				cb(d)
			}
		case <-m.closed:
			return
		}
	}
}

func (m *memLink) Connect(context.Context) error { return nil }
func (m *memLink) Send(data []byte) error {
	cp := append([]byte(nil), data...)
	select {
	case m.peer.ch <- cp:
		return nil
	case <-m.peer.closed:
		return io.ErrClosedPipe
	}
}
func (m *memLink) Close() error {
	select {
	case <-m.closed:
	default:
		close(m.closed)
	}
	return nil
}
func (m *memLink) SetReconnectCallback(func())     {}
func (m *memLink) SetShouldReconnect(func() bool)  {}
func (m *memLink) SetEndedCallback(func(string))   {}
func (m *memLink) WatchConnection(context.Context) {}
func (m *memLink) CanSend() bool {
	select {
	case <-m.closed:
		return false
	default:
		return true
	}
}
func (m *memLink) Features() transport.Features {
	return transport.Features{Reliable: true, Ordered: true, MessageOriented: true}
}
func (m *memLink) Reconnect(string) {}

// memCtrlLink wraps a memLink with an isolated control plane (like
// vp8channel): control frames ride a separate channel from data, so a bond
// over it takes the control-plane handshake path (acceptBondHandshake +
// sessionReady).
type memCtrlLink struct {
	*memLink
	ctlCh   chan []byte
	ctlPeer *memCtrlLink
	ctlOnce sync.Once
	ctlMu   sync.Mutex
	ctlOn   func([]byte)
}

func newMemCtrlLinkPair() (*memCtrlLink, *memCtrlLink) {
	a := &memCtrlLink{ctlCh: make(chan []byte, 512)}
	b := &memCtrlLink{ctlCh: make(chan []byte, 512)}
	a.memLink, b.memLink = newMemLinkPair()
	a.memLink.peer = b.memLink
	b.memLink.peer = a.memLink
	a.ctlPeer = b
	b.ctlPeer = a
	return a, b
}

func (m *memCtrlLink) start() {
	m.memLink.start()
	m.ctlOnce.Do(func() { go m.pumpCtl() })
}

func (m *memCtrlLink) pumpCtl() {
	for {
		select {
		case d := <-m.ctlCh:
			m.ctlMu.Lock()
			cb := m.ctlOn
			m.ctlMu.Unlock()
			if cb != nil {
				cb(d)
			}
		case <-m.closed:
			return
		}
	}
}

func (m *memCtrlLink) ControlSend(data []byte) error {
	cp := append([]byte(nil), data...)
	select {
	case m.ctlPeer.ctlCh <- cp:
		return nil
	case <-m.ctlPeer.closed:
		return io.ErrClosedPipe
	}
}
func (m *memCtrlLink) SetControlOnData(cb func([]byte)) {
	m.ctlMu.Lock()
	m.ctlOn = cb
	m.ctlMu.Unlock()
}
func (m *memCtrlLink) ControlCanSend() bool { return m.CanSend() }

var (
	_ transport.Transport    = (*memLink)(nil)
	_ transport.ControlPlane = (*memCtrlLink)(nil)
)

// ---------------------------------------------------------------------------
// Frame builders and shared server/client helpers.
// ---------------------------------------------------------------------------

// bondHelloFrame builds a PATH_HELLO wire frame announcing path idx of id.
func bondHelloFrame(id [16]byte, idx uint16) []byte {
	buf := make([]byte, 1+16+2+2)
	buf[0] = 1
	copy(buf[1:17], id[:])
	binary.BigEndian.PutUint16(buf[17:19], idx)
	binary.BigEndian.PutUint16(buf[19:21], 1) // numPaths
	return buf
}

// bondDataFrame builds a DATA wire frame carrying seq/payload.
func bondDataFrame(seq uint64, payload []byte) []byte {
	buf := make([]byte, 1+8+len(payload))
	buf[0] = 2
	binary.BigEndian.PutUint64(buf[1:9], seq)
	copy(buf[9:], payload)
	return buf
}

func dropBondFrame([]byte) {}

func testCipher(t *testing.T) *cryptopkg.Cipher {
	t.Helper()
	c, err := cryptopkg.NewCipher("01234567890123456789012345678901")
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	return c
}

// newBondTestServer builds the Server surface a bond session touches. On
// teardown it closes every live bond so per-bond accept/serve goroutines exit.
func newBondTestServer(t *testing.T, cipher *cryptopkg.Cipher) *Server {
	t.Helper()
	s := &Server{
		baseCtx:      context.Background(),
		cipher:       cipher,
		authHook:     defaultAuthHook,
		onOpen:       func(string, string, map[string]any) {},
		onClose:      func(string, string) {},
		onTraffic:    func(string, string, uint64, uint64) {},
		liveness:     control.Config{},
		health:       runtime.NewHealthTracker(nil),
		done:         make(chan struct{}),
		bondMode:     make(chan struct{}),
		bonds:        make(map[[16]byte]*bondEntry),
		peerStats:    make(map[string]peerStat),
		peerSessions: make(map[string]*peerSession),
	}
	t.Cleanup(func() {
		s.bondMu.Lock()
		ids := make([][16]byte, 0, len(s.bonds))
		for id := range s.bonds {
			ids = append(ids, id)
		}
		s.bondMu.Unlock()
		for _, id := range ids {
			s.removeBond(id)
		}
	})
	return s
}

func waitForBond(t *testing.T, s *Server, id [16]byte) *bondEntry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.bondMu.Lock()
		be := s.bonds[id]
		s.bondMu.Unlock()
		if be != nil {
			return be
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("bond %x never registered", id)
	return nil
}

// waitBondSession waits until the bond's per-bond session (with a control
// session, i.e. the sessionReady path) is fully brought up.
func waitBondSession(t *testing.T, s *Server, id [16]byte) *bondEntry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.bondMu.Lock()
		be := s.bonds[id]
		up := be != nil && be.sess != nil && be.sess.controlSess != nil
		s.bondMu.Unlock()
		if up {
			return be
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("bond %x control session never started", id)
	return nil
}

func waitBondRemoved(t *testing.T, s *Server, id [16]byte) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.bondMu.Lock()
		be := s.bonds[id]
		s.bondMu.Unlock()
		if be == nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// testBondClient is a client-side bond session: it opens the handshake stream
// (on the isolated control session when the carrier has one, on the data
// session otherwise - mirroring the real client) and can open tunnel streams.
type testBondClient struct {
	bond    *multipath.Bond
	carrier transport.Transport
	sess    *smux.Session
	ctlSess *smux.Session
	ctlConn *muxconn.Conn
}

func startTestBondClient(t *testing.T, cipher *cryptopkg.Cipher, id [16]byte, link transport.Transport) *testBondClient {
	t.Helper()
	bond := multipath.NewBond(id, multipath.RoleClient)
	bond.AddPath(link, 0)
	// Route inbound carrier frames into the bond (the real client wires this
	// via transport.Config.OnData = bond.PathOnData).
	if l, ok := link.(interface{ setOnData(func([]byte)) }); ok {
		l.setOnData(bond.PathOnData(0))
	}
	if err := bond.Connect(context.Background()); err != nil {
		t.Fatalf("client bond connect: %v", err)
	}
	carrier := multipath.AsCarrier(bond)
	conn := muxconn.New(carrier, cipher)
	if sink, ok := carrier.(interface{ SetOnData(func([]byte)) }); ok {
		sink.SetOnData(conn.Push)
	}
	sess, err := smux.Client(conn, runtime.SmuxConfigFor(carrier))
	if err != nil {
		t.Fatalf("smux client: %v", err)
	}
	cli := &testBondClient{bond: bond, carrier: carrier, sess: sess}
	if ctrlConn := muxconn.NewControl(carrier, cipher); ctrlConn != nil {
		cli.ctlConn = ctrlConn
		cli.ctlSess, err = smux.Client(ctrlConn, controlSmuxConfig(linkMaxPayload(carrier)))
		if err != nil {
			t.Fatalf("control smux client: %v", err)
		}
	}
	t.Cleanup(func() {
		_ = sess.Close()
		if cli.ctlSess != nil {
			_ = cli.ctlSess.Close()
		}
		_ = conn.Close()
		if cli.ctlConn != nil {
			_ = cli.ctlConn.Close()
		}
		_ = bond.Close()
	})
	cli.handshake(t, "test-device", nil)
	return cli
}

func (c *testBondClient) handshake(t *testing.T, deviceID string, claims map[string]any) string {
	t.Helper()
	hs := c.sess
	if c.ctlSess != nil {
		hs = c.ctlSess
	}
	stream, err := hs.OpenStream()
	if err != nil {
		t.Fatalf("open handshake stream: %v", err)
	}
	_ = stream.SetDeadline(time.Now().Add(handshake.DefaultTimeout))
	sid, err := handshake.Client(stream, deviceID, claims)
	_ = stream.SetDeadline(time.Time{})
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	return sid
}

// roundTrip drives one connect request through a freshly opened tunnel stream
// to the echo server, proving the bond's streams are accepted and served.
func (c *testBondClient) roundTrip(t *testing.T, host string, port int) {
	t.Helper()
	stream, err := c.sess.OpenStream()
	if err != nil {
		t.Fatalf("open tunnel stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	req := fmt.Sprintf(`{"cmd":"connect","addr":%q,"port":%d}`, host, port)
	_ = stream.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := stream.Write([]byte(req)); err != nil {
		t.Fatalf("write connect req: %v", err)
	}
	_ = stream.SetWriteDeadline(time.Time{})
	_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	ack := make([]byte, 1)
	if _, err := io.ReadFull(stream, ack); err != nil {
		t.Fatalf("read connect ack: %v", err)
	}
	payload := []byte("ping-through-bond")
	if _, err := stream.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo mismatch: got %q want %q", got, payload)
	}
}

// startEchoServer returns a TCP echo listener address.
func startEchoServer(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

// ai-generated: regression test for the ended-teardown semantics (issue A).
// With a single bond its death cancels the run (as before); with several
// concurrent bonds one death must only drop that bond; peer bonds never cancel.
func TestBondEnded_LastBondCancelsRun(t *testing.T) {
	cipher := testCipher(t)
	s := newBondTestServer(t, cipher)
	var cancelled atomic.Int32
	s.bondCancel = func() { cancelled.Store(1) }

	mk := func(suffix byte) *bondEntry {
		id := [16]byte{suffix}
		be, _ := s.getOrCreateBond(id)
		s.addBondPath(be, &serverLinkStub{}, 0)
		return be
	}
	be1 := mk(0x1)
	be2 := mk(0x2)

	// One of two bonds ends: the run survives and the other bond stays.
	s.bondEnded(be1, false)
	if cancelled.Load() != 0 {
		t.Fatal("one of several bonds ending must not cancel the run")
	}
	s.bondMu.Lock()
	left := s.bonds[[16]byte{0x2}] != nil
	s.bondMu.Unlock()
	if !left {
		t.Fatal("surviving bond was removed together with the dead one")
	}

	// The last bond ends: the run is cancelled, like the single-bond case.
	s.bondEnded(be2, false)
	if cancelled.Load() != 1 {
		t.Fatal("last bond ending must cancel the run")
	}

	// A peer bond's death never cancels the run.
	be3 := mk(0x3)
	s.bondEnded(be3, true)
	if cancelled.Load() != 1 {
		t.Fatal("peer bond ending must not cancel the run")
	}
}

// ---------------------------------------------------------------------------
// Regression tests.
// ---------------------------------------------------------------------------

// ai-generated: regression test for issue A (two successive non-peer bonds).
// The first bond's session must stay live and independently served when the
// second bond starts, and bonded mode must not install a singleton s.session
// at all (the old code overwrote the singleton and orphaned the first bond's
// smux session).
func TestBondSession_MultiClientNoOverwrite(t *testing.T) {
	cipher := testCipher(t)
	s := newBondTestServer(t, cipher)

	// Two clients, two bond ids, two carriers - the multi-client multipath
	// shape. Each carrier classifies independently (like bringUpMultipath).
	idA := multipath.NewBondID()
	srvA, cliA := newMemLinkPair()
	var routerA atomic.Pointer[func([]byte)]
	srvA.setOnData(func(d []byte) { s.routeBondFrame(srvA, &routerA, d, dropBondFrame) })
	srvA.start()
	cliA.start()
	clientA := startTestBondClient(t, cipher, idA, cliA)

	idB := multipath.NewBondID()
	srvB, cliB := newMemLinkPair()
	var routerB atomic.Pointer[func([]byte)]
	srvB.setOnData(func(d []byte) { s.routeBondFrame(srvB, &routerB, d, dropBondFrame) })
	srvB.start()
	cliB.start()
	clientB := startTestBondClient(t, cipher, idB, cliB)

	beA := waitForBond(t, s, idA)
	beB := waitForBond(t, s, idB)
	if beA.sess == nil {
		t.Fatal("bond A has no per-bond session")
	}
	if beB.sess == nil {
		t.Fatal("bond B has no per-bond session")
	}
	if beA.sess == beB.sess {
		t.Fatal("bonds share a session; the second bond overwrote the first")
	}
	s.sessMu.RLock()
	singleton := s.session
	s.sessMu.RUnlock()
	if singleton != nil {
		t.Fatal("bonded mode must not install a singleton s.session")
	}

	// Both sessions must actually serve tunnel streams end to end, in both
	// orders - first A (orphaned by the old code), then B, then A again.
	host, port := startEchoServer(t)
	clientA.roundTrip(t, host, port)
	clientB.roundTrip(t, host, port)
	clientA.roundTrip(t, host, port)
}

// ai-generated: regression test for issue B. A failed bond control-plane
// handshake must close sessionReady, waking the serveBond goroutine blocked in
// waitBondHandshake - otherwise the waiting goroutine never exits until server
// shutdown. The test sends a well-formed but rejected CLIENT_HELLO (bad
// protocol version) and then requires the server's wg to drain within a
// timeout.
func TestBondHandshakeFailure_ClosesSessionReady(t *testing.T) {
	cipher := testCipher(t)
	s := newBondTestServer(t, cipher)

	srv, cli := newMemCtrlLinkPair()
	var router atomic.Pointer[func([]byte)]
	srv.setOnData(func(d []byte) { s.routeBondFrame(srv, &router, d, dropBondFrame) })
	srv.start()
	cli.start()

	id := multipath.NewBondID()
	bond := multipath.NewBond(id, multipath.RoleClient)
	bond.AddPath(cli, 0)
	if err := bond.Connect(context.Background()); err != nil {
		t.Fatalf("client bond connect: %v", err)
	}
	t.Cleanup(func() { _ = bond.Close() })

	_ = waitBondSession(t, s, id)

	// Drive a FAILED handshake on the client's control session: open the
	// control stream and send a well-formed but rejected CLIENT_HELLO.
	carrier := multipath.AsCarrier(bond)
	ctrlConn := muxconn.NewControl(carrier, cipher)
	if ctrlConn == nil {
		t.Fatal("client control conn expected")
	}
	ctrlSess, err := smux.Client(ctrlConn, controlSmuxConfig(linkMaxPayload(carrier)))
	if err != nil {
		t.Fatalf("control smux client: %v", err)
	}
	stream, err := ctrlSess.OpenStream()
	if err != nil {
		t.Fatalf("open control stream: %v", err)
	}
	_ = stream.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := framing.WriteJSON(stream, handshake.Hello{Version: 99, Type: handshake.TypeHello, DeviceID: "dev"}, handshake.MaxMessageSize); err != nil {
		t.Fatalf("write bad hello: %v", err)
	}
	_ = stream.SetWriteDeadline(time.Time{})
	_ = stream.Close()
	_ = ctrlSess.Close()
	_ = ctrlConn.Close()

	if !waitBondRemoved(t, s, id) {
		t.Fatal("failed handshake should remove the bond")
	}

	// The acceptBondHandshake and serveBond goroutines must both exit; with the
	// leak, serveBond spins in waitBondHandshake and wg.Wait never returns.
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bond handshake goroutines leaked: wg.Wait not reached")
	}
}

// peerCarrierStub is a peer-routing carrier stub for driving the peer-carrier
// demux (Phase 5b routing only - no real data plane needed).
type peerCarrierStub struct {
	*serverLinkStub
}

func (p *peerCarrierStub) SendTo(peerID string, data []byte) error {
	return p.serverLinkStub.Send(data)
}
func (p *peerCarrierStub) SupportsPeerRouting() bool { return true }
func (p *peerCarrierStub) Features() transport.Features {
	return transport.Features{Reliable: true, Ordered: true, MessageOriented: true}
}

// ai-generated: regression test for issue C part 1 (stale routes). When a
// peer-routed bond dies, removeBond must drop every demux route pointing at it
// so a retry from the same peerID registers a fresh bond, and any frame still
// arriving for the dead bond's path is dropped instead of routed into the
// closed bond's handler.
func TestPeerDemux_DropsStaleRoutesOnBondDeath(t *testing.T) {
	cipher := testCipher(t)
	s := newBondTestServer(t, cipher)

	d := newPeerCarrierDemux(s)
	d.tr = &peerCarrierStub{serverLinkStub: &serverLinkStub{}}
	s.bondMu.Lock()
	s.demuxes = append(s.demuxes, d)
	s.bondMu.Unlock()

	const peerID = "peer-1"

	// First PATH_HELLO resolves the peer to bond A.
	idA := multipath.NewBondID()
	s.routePeerBondData(d, peerID, bondHelloFrame(idA, 0))
	beA := s.bonds[idA]
	if beA == nil {
		t.Fatal("first hello did not create a bond")
	}
	d.mu.Lock()
	rA := d.routes[peerID]
	d.mu.Unlock()
	if rA == nil || rA.be != beA {
		t.Fatal("route should resolve to bond A")
	}

	// Bond death: removeBond must drop the route so a retry can re-register.
	s.removeBond(idA)
	d.mu.Lock()
	stale := d.routes[peerID]
	d.mu.Unlock()
	if stale != nil {
		t.Fatal("stale route survived bond removal")
	}

	// A stale frame (not a fresh PATH_HELLO) must be dropped, not routed.
	s.routePeerBondData(d, peerID, bondDataFrame(1, []byte("stale")))
	d.mu.Lock()
	if d.routes[peerID] != nil {
		t.Fatal("stale frame re-registered a route")
	}
	d.mu.Unlock()

	// A retry with a fresh bond id re-registers the route and brings up a new
	// session.
	idB := multipath.NewBondID()
	s.routePeerBondData(d, peerID, bondHelloFrame(idB, 0))
	beB := s.bonds[idB]
	if beB == nil {
		t.Fatal("retry did not create a fresh bond")
	}
	d.mu.Lock()
	rB := d.routes[peerID]
	d.mu.Unlock()
	if rB == nil || rB.be != beB {
		t.Fatal("retry did not re-register the route")
	}
	if beB.sess == nil {
		t.Fatal("retried bond has no session")
	}
}

// ai-generated: regression test for issue C part 2 (unbounded pre-hello
// control buffering). Buffered control frames are bounded in aggregate across
// all peers (maxBufferedPeerControlTotal); past the cap new frames are dropped
// and resolving a route flushes (and decrements) that peer's queue.
func TestPeerDemux_CtlBufGlobalCap(t *testing.T) {
	cipher := testCipher(t)
	s := newBondTestServer(t, cipher)
	d := newPeerCarrierDemux(s)
	d.tr = &peerCarrierStub{serverLinkStub: &serverLinkStub{}}

	// 200 never-hello peers x 10 control frames each: well past both the
	// per-peer cap (not hit) and the aggregate cap, so the buffer must stop at
	// the aggregate cap.
	for i := 0; i < 200; i++ {
		for j := 0; j < 10; j++ {
			s.routePeerBondControl(d, fmt.Sprintf("peer-%d", i), []byte("ctl"))
		}
	}
	d.mu.Lock()
	total := 0
	for _, frames := range d.ctlBuf {
		total += len(frames)
	}
	bufTotal := d.ctlTotal
	queuedPeer0 := len(d.ctlBuf["peer-0"])
	d.mu.Unlock()
	if bufTotal != maxBufferedPeerControlTotal {
		t.Fatalf("ctlTotal = %d, want %d", bufTotal, maxBufferedPeerControlTotal)
	}
	if total > bufTotal {
		t.Fatalf("buffered frames %d exceed ctlTotal %d", total, bufTotal)
	}

	// A PATH_HELLO for one buffered peer flushes its queue and must decrement
	// the aggregate counter.
	id := multipath.NewBondID()
	s.routePeerBondData(d, "peer-0", bondHelloFrame(id, 0))
	d.mu.Lock()
	flushed := len(d.ctlBuf["peer-0"])
	after := d.ctlTotal
	d.mu.Unlock()
	if flushed != 0 {
		t.Fatalf("route resolution did not flush peer-0's buffer (%d frames left)", flushed)
	}
	if want := bufTotal - queuedPeer0; after != want {
		t.Fatalf("ctlTotal after flush = %d, want %d", after, want)
	}
}
