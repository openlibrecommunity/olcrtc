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

var _ transport.Transport = (*memLink)(nil)

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
