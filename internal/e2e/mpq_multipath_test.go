package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/app/session"
	"github.com/openlibrecommunity/olcrtc/internal/client"
	"github.com/openlibrecommunity/olcrtc/internal/engine"
	enginebuiltin "github.com/openlibrecommunity/olcrtc/internal/engine/builtin"
	"github.com/openlibrecommunity/olcrtc/internal/server"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/pion/webrtc/v4"
)

// --- Peer-routing in-memory carrier for mpq multipath ---
//
// This models a real peer-routing carrier (vp8channel/datachannel over an SFU):
// client carriers and server carriers share a room; the server distinguishes
// each client by a stable peerID. It is the substrate the mpq server demuxes
// over (mpqx.RoomConn, one carrier per configured room). It is deliberately
// separate from the broadcast memoryStream used by the legacy tests: multipath
// REQUIRES per-peer routing (a broadcast carrier would cross-feed the paths'
// QUIC conns).
//
// A carrier registered with OnPeerData set is a server end; one registered with
// only OnData set is a client end and is assigned a unique peerID. A client Send
// arrives at the server as OnPeerData(peerID, data); the server's SendTo(peerID,
// data) is delivered back to that one client's OnData. Every stream belongs to
// the room its transport was opened in, so different rooms are isolated message
// domains.

// ai-generated: hub re-keyed per room, phase 1 multi-room mpq. mpqPeerHub hosts
// one message domain per room: a server and its clients sharing a room exchange
// frames ONLY within that room, so a "separate calls" client whose N paths live
// in N different rooms gets N isolated domains on the server side, and a
// single-room multi-path client shares one domain. Each room holds one server
// end (peerID "") and any number of client ends.
type mpqPeerHub struct {
	mu     sync.Mutex
	rooms  map[string]*mpqPeerRoom
	nextID atomic.Int64
}

// ai-generated: new type, phase 1 multi-room mpq. mpqPeerRoom is one isolated
// room: the server end plus every client end in it.
type mpqPeerRoom struct {
	server   *mpqPeerStream
	clients  map[string]*mpqPeerStream
	serverUp chan struct{} // closed once the server end is connected
}

func newMPQPeerHub() *mpqPeerHub {
	return &mpqPeerHub{rooms: make(map[string]*mpqPeerRoom)}
}

// ai-generated: new method, phase 1 multi-room mpq. roomLocked returns the room
// entry for name, creating it on first use. Callers must hold h.mu.
func (h *mpqPeerHub) roomLocked(name string) *mpqPeerRoom {
	r := h.rooms[name]
	if r == nil {
		r = &mpqPeerRoom{clients: make(map[string]*mpqPeerStream), serverUp: make(chan struct{})}
		h.rooms[name] = r
	}
	return r
}

// ai-generated: new type, phase 1 multi-room mpq. mpqPeerStream is one carrier
// end in a room: room + recvBytes fields were added (recvBytes counts bytes
// this stream received from the server via SendTo, so tests can assert a
// specific client path actually moved traffic).
type mpqPeerStream struct {
	hub        *mpqPeerHub
	room       string // room URL this stream belongs to (the message domain)
	peerID     string // "" on the server end
	isServer   bool
	onData     func([]byte)
	onPeerData func(peerID string, data []byte)

	mu        sync.Mutex
	connected bool
	closed    bool
	// dead makes a client path silently drop everything in both directions,
	// simulating a killed path without tearing the carrier down.
	dead  bool
	ended func(string)
	// recvBytes counts bytes this stream received from the server (SendTo), so
	// tests can assert a specific client path actually moved traffic.
	recvBytes atomic.Int64
}

func (s *mpqPeerStream) Connect(context.Context) error {
	s.mu.Lock()
	s.connected = true
	s.mu.Unlock()
	if s.isServer {
		s.hub.mu.Lock()
		room := s.hub.roomLocked(s.room)
		room.server = s
		serverUp := room.serverUp
		s.hub.mu.Unlock()
		select {
		case <-serverUp:
		default:
			close(serverUp)
		}
	}
	return nil
}

// Send: client -> server (tagged with our peerID). The server end never calls
// Send (it uses SendTo), so on the server this is a no-op.
func (s *mpqPeerStream) Send(data []byte) error {
	if s.isServer {
		return nil
	}
	s.mu.Lock()
	dead := s.dead || s.closed
	s.mu.Unlock()
	if dead {
		return nil
	}
	s.hub.mu.Lock()
	room := s.hub.roomLocked(s.room)
	srv := room.server
	s.hub.mu.Unlock()
	if srv == nil {
		return nil
	}
	cp := append([]byte(nil), data...)
	srv.mu.Lock()
	cb := srv.onPeerData
	srv.mu.Unlock()
	if cb != nil {
		cb(s.peerID, cp)
	}
	return nil
}

// SendTo: server -> a specific client. Implements engine.PeerSession, which is
// what makes the server transport report SupportsPeerRouting()==true.
func (s *mpqPeerStream) SendTo(peerID string, data []byte) error {
	s.hub.mu.Lock()
	room := s.hub.roomLocked(s.room)
	c := room.clients[peerID]
	s.hub.mu.Unlock()
	if c == nil {
		return nil
	}
	c.mu.Lock()
	dead := c.dead || c.closed
	cb := c.onData
	c.mu.Unlock()
	if dead || cb == nil {
		return nil
	}
	c.recvBytes.Add(int64(len(data)))
	cb(append([]byte(nil), data...))
	return nil
}

// WaitForPeer (engine.PeerReadySession): a client waits until the server end of
// ITS room is up. The server end returns immediately.
func (s *mpqPeerStream) WaitForPeer(ctx context.Context) error {
	if s.isServer {
		return nil
	}
	s.hub.mu.Lock()
	room := s.hub.roomLocked(s.room)
	serverUp := room.serverUp
	s.hub.mu.Unlock()
	select {
	case <-serverUp:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *mpqPeerStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.connected = false
	s.mu.Unlock()
	return nil
}

// kill simulates a dead path: both directions silently drop.
func (s *mpqPeerStream) kill() {
	s.mu.Lock()
	s.dead = true
	s.mu.Unlock()
}

// endConference models a room/carrier that has died AND announced it via the
// carrier's conference-end callback (SetEndedCallback), the way a real
// vp8channel/datachannel carrier signals its room going away. Both directions
// go dead (like kill) and the ended callback fires, which drives the client to
// proactively fail this path in the mpq session (Session.FailPath) instead of
// waiting out the QUIC idle timeout.
func (s *mpqPeerStream) endConference(reason string) {
	s.mu.Lock()
	s.dead = true
	ended := s.ended
	s.mu.Unlock()
	if ended != nil {
		ended(reason)
	}
}

func (s *mpqPeerStream) SetReconnectCallback(func(*webrtc.DataChannel)) {}
func (s *mpqPeerStream) SetShouldReconnect(func() bool)                 {}
func (s *mpqPeerStream) SetEndedCallback(cb func(string)) {
	s.mu.Lock()
	s.ended = cb
	s.mu.Unlock()
}
func (s *mpqPeerStream) WatchConnection(ctx context.Context) { <-ctx.Done() }
func (s *mpqPeerStream) CanSend() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connected && !s.closed
}
func (s *mpqPeerStream) SubscriberCanSend() bool   { return s.CanSend() }
func (s *mpqPeerStream) GetSendQueue() chan []byte { return nil }
func (s *mpqPeerStream) GetBufferedAmount() uint64 { return 0 }
func (s *mpqPeerStream) Reconnect(string)          {}
func (s *mpqPeerStream) Capabilities() engine.Capabilities {
	return engine.Capabilities{ByteStream: true}
}

// registerMPQPeerCarrier registers a peer-routing carrier engine keyed by room
// URL and returns its name plus the hub, so a test can reach individual client
// paths (e.g. to kill one). A stream belongs to the room its transport was
// opened in (enginebuiltin.Config.RoomURL), keeping rooms isolated message
// domains.
func registerMPQPeerCarrier(t *testing.T) (string, *mpqPeerHub) {
	t.Helper()
	session.RegisterDefaults()
	hub := newMPQPeerHub()
	name := "e2e-mpq-peer-" + t.Name()
	enginebuiltin.Register(name, func(_ context.Context, cfg enginebuiltin.Config) (engine.Session, error) {
		s := &mpqPeerStream{
			hub:        hub,
			room:       cfg.RoomURL,
			onData:     cfg.OnData,
			onPeerData: cfg.OnPeerData,
		}
		if cfg.OnPeerData != nil {
			s.isServer = true
		} else {
			s.peerID = fmt.Sprintf("mpq-client-%d", hub.nextID.Add(1))
			hub.mu.Lock()
			hub.roomLocked(s.room).clients[s.peerID] = s
			hub.mu.Unlock()
		}
		return s, nil
	})
	return name, hub
}

// twoPaths builds two identical path specs pointing at the same peer-routing
// room, so the client opens two carriers (two peerIDs) that mpq bonds into one
// session and the server demuxes back apart.
func twoPaths() []transport.PathSpec {
	return []transport.PathSpec{
		{Transport: transportData, RoomURL: testRoom},
		{Transport: transportData, RoomURL: testRoom},
	}
}

// startMPQMultipathTunnel brings up an mpq server + a 2-path mpq client over the
// peer-routing in-memory carrier and returns the runtime plus the hub.
func startMPQMultipathTunnel(t *testing.T) (*tunnelRuntime, *mpqPeerHub) {
	t.Helper()

	carrierName, hub := registerMPQPeerCarrier(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	socksAddr := freeLocalAddr(ctx, t)

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Run(ctx, server.Config{
			Transport:      transportData,
			TransportProto: "mpq",
			Carrier:        carrierName,
			RoomURL:        testRoom,
			KeyHex:         testKeyHex,
			DNSServer:      localDNSServer,
			// Two paths per client: sets the mpq Listener's ExpectedPaths so a
			// session finalises promptly once both paths are present.
			Paths: twoPaths(),
		})
	}()

	// Wait for the server carrier to connect before starting the client (the
	// client's QUIC handshake must land on a live listener).
	hub.mu.Lock()
	serverUp := hub.roomLocked(testRoom).serverUp
	hub.mu.Unlock()
	select {
	case <-serverUp:
	case err := <-serverErr:
		t.Fatalf("mpq server exited before connecting: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("mpq server carrier did not connect")
	}

	ready := make(chan struct{})
	clientErr := make(chan error, 1)
	go func() {
		clientErr <- client.RunWithReady(ctx, client.Config{
			Transport:      transportData,
			TransportProto: "mpq",
			Carrier:        carrierName,
			RoomURL:        testRoom,
			KeyHex:         testKeyHex,
			DeviceID:       testClientDeviceID,
			LocalAddr:      socksAddr,
			DNSServer:      localDNSServer,
			Paths:          twoPaths(),
		}, func() { close(ready) })
	}()
	waitForReadyWithin(t, ready, 15*time.Second)

	return &tunnelRuntime{
		socksAddr: socksAddr,
		cancel:    cancel,
		serverErr: serverErr,
		clientErr: clientErr,
		stopWait:  10 * time.Second,
	}, hub
}

// echoRoundTrip writes payload through the SOCKS tunnel to the echo server and
// verifies it comes back byte-for-byte within budget.
func echoRoundTrip(t *testing.T, conn interface {
	io.ReadWriter
	SetDeadline(time.Time) error
}, size int, budget time.Duration) {
	t.Helper()
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("gen payload: %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(budget)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	echoed := make([]byte, len(payload))
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(conn, echoed)
		done <- err
	}()
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !bytes.Equal(echoed, payload) {
		t.Fatalf("echo mismatch (%d bytes)", len(payload))
	}
}

// TestMPQMultipathSOCKSTunnel proves the mpq tunnel carries real SOCKS data
// end-to-end when split across two carrier paths bonded into one QUIC session
// and demuxed on the server by peerID.
func TestMPQMultipathSOCKSTunnel(t *testing.T) {
	echoAddr := startEchoServer(t)
	rt, hub := startMPQMultipathTunnel(t)
	defer rt.stop(t)

	// Sanity: the client really opened two distinct carrier paths.
	hub.mu.Lock()
	nClients := len(hub.roomLocked(testRoom).clients)
	hub.mu.Unlock()
	if nClients != 2 {
		t.Fatalf("expected 2 client carrier paths, got %d", nClients)
	}

	conn := connectViaSOCKS(t, rt.socksAddr, echoAddr)
	defer func() { _ = conn.Close() }()

	// Small round-trip then a bulk transfer, both over the bonded session.
	echoRoundTrip(t, conn, 4096, 10*time.Second)
	echoRoundTrip(t, conn, 256*1024, 30*time.Second)
}

// TestMPQMultipathSurvivesPathDrop silently kills one of the two paths mid-tunnel
// (both directions drop, no carrier callback) and verifies data still flows over
// the surviving path. With no liveness signal from the carrier, the only thing
// that retires the dead path is the QUIC idle timeout — now shrunk to a few
// seconds (mpqx.PathMaxIdleTimeout) instead of mpq-brutal's ~45s UDP default. So
// this used to take ~45-55s and now completes in a handful of seconds; the tight
// budget below is the direct assertion of fast dead-path detection.
func TestMPQMultipathSurvivesPathDrop(t *testing.T) {
	echoAddr := startEchoServer(t)
	rt, hub := startMPQMultipathTunnel(t)
	defer rt.stop(t)

	conn := connectViaSOCKS(t, rt.socksAddr, echoAddr)
	defer func() { _ = conn.Close() }()

	// Warm up on both paths.
	echoRoundTrip(t, conn, 4096, 10*time.Second)

	// Kill one path (silently drop both directions). The other must carry on.
	hub.mu.Lock()
	var victim *mpqPeerStream
	for _, c := range hub.roomLocked(testRoom).clients {
		victim = c
		break
	}
	hub.mu.Unlock()
	if victim == nil {
		t.Fatal("no client path to kill")
	}
	victim.kill()
	t.Logf("killed path peerID=%s", victim.peerID)

	// Data must still round-trip via the surviving path. The dead path is retired
	// at the (now small) QUIC idle timeout, well under the old ~45s; a 25s budget
	// proves detection is fast while leaving slack for CI jitter and loss recovery.
	start := time.Now()
	echoRoundTrip(t, conn, 64*1024, 25*time.Second)
	t.Logf("survived path drop, round-trip completed in %s", time.Since(start))
}

// TestMPQMultipathProactivePathRemoval ends one path's conference (fires the
// carrier's SetEndedCallback, the way a real room-death is signalled) and
// verifies the client proactively fails that path in the mpq session and keeps
// serving data over the survivor. Firing the ended callback exercises the
// carrier-liveness wiring (wireMPQPathLiveness -> Session.FailPath), which evicts
// the dead path immediately instead of waiting out the QUIC idle timeout.
//
// The post-eviction round-trips are kept small on purpose: each fits in a single
// mpq chunk, so recovery of anything that raced onto the dead path rides fast RTO
// retransmission onto the survivor (~tens of ms) rather than a bulk transfer that
// would also depend on the server side detecting its own dead path. This isolates
// what we want to assert here — the client's proactive-eviction wiring keeps the
// tunnel serving — from the separate bulk loss-recovery timing exercised by
// TestMPQMultipathSurvivesPathDrop.
func TestMPQMultipathProactivePathRemoval(t *testing.T) {
	echoAddr := startEchoServer(t)
	rt, hub := startMPQMultipathTunnel(t)
	defer rt.stop(t)

	conn := connectViaSOCKS(t, rt.socksAddr, echoAddr)
	defer func() { _ = conn.Close() }()

	// Warm up on both paths.
	echoRoundTrip(t, conn, 4096, 10*time.Second)

	// End one path's conference: dead in both directions AND announced via the
	// ended callback, which drives proactive FailPath on the client.
	hub.mu.Lock()
	var victim *mpqPeerStream
	for _, c := range hub.roomLocked(testRoom).clients {
		victim = c
		break
	}
	hub.mu.Unlock()
	if victim == nil {
		t.Fatal("no client path to end")
	}
	victim.endConference("room closed")
	t.Logf("ended conference on path peerID=%s", victim.peerID)

	// The surviving path must keep the tunnel up. Proactive eviction takes the
	// dead path out of the scheduler immediately; a couple of small round-trips
	// confirm data keeps flowing over the survivor promptly.
	start := time.Now()
	echoRoundTrip(t, conn, 4096, 15*time.Second)
	echoRoundTrip(t, conn, 4096, 15*time.Second)
	t.Logf("served over survivor after proactive eviction in %s", time.Since(start))
}

// --- Multi-room mpq: separate calls, one bond per client across rooms ---
//
// ai-generated: the twoRoomPaths/startMPQMultiRoomTunnel/roomClientTraffic
// helpers and TestMPQMultiRoomSeparateCalls below, phase 1 multi-room mpq.

// twoRoomPaths builds two path specs pointing at two DIFFERENT rooms, so a
// client bonds two paths that live in separate message domains (the "separate
// calls" mode) and the server hosts one carrier per room.
func twoRoomPaths() []transport.PathSpec {
	return []transport.PathSpec{
		{Transport: transportData, RoomURL: testRoom + "-a"},
		{Transport: transportData, RoomURL: testRoom + "-b"},
	}
}

// startMPQMultiRoomTunnel brings up an mpq server + a 2-path mpq client where
// each path lives in its OWN room, and returns the runtime plus the hub. The
// server is configured with the same two rooms, so it raises one carrier per
// room and bonds them through one RoomConn.
func startMPQMultiRoomTunnel(t *testing.T) (*tunnelRuntime, *mpqPeerHub) {
	t.Helper()

	carrierName, hub := registerMPQPeerCarrier(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	socksAddr := freeLocalAddr(ctx, t)

	paths := twoRoomPaths()
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Run(ctx, server.Config{
			Transport:      transportData,
			TransportProto: "mpq",
			Carrier:        carrierName,
			KeyHex:         testKeyHex,
			DNSServer:      localDNSServer,
			Paths:          paths,
		})
	}()

	// Wait for the server carrier of EVERY room before starting the client (its
	// QUIC handshake must land on a live listener in each room).
	for _, ps := range paths {
		hub.mu.Lock()
		serverUp := hub.roomLocked(ps.RoomURL).serverUp
		hub.mu.Unlock()
		select {
		case <-serverUp:
		case err := <-serverErr:
			t.Fatalf("mpq server exited before connecting room %s: %v", ps.RoomURL, err)
		case <-time.After(5 * time.Second):
			t.Fatalf("mpq server carrier for room %s did not connect", ps.RoomURL)
		}
	}

	ready := make(chan struct{})
	clientErr := make(chan error, 1)
	go func() {
		clientErr <- client.RunWithReady(ctx, client.Config{
			Transport:      transportData,
			TransportProto: "mpq",
			Carrier:        carrierName,
			DeviceID:       testClientDeviceID,
			LocalAddr:      socksAddr,
			KeyHex:         testKeyHex,
			DNSServer:      localDNSServer,
			Paths:          paths,
		}, func() { close(ready) })
	}()
	waitForReadyWithin(t, ready, 15*time.Second)

	return &tunnelRuntime{
		socksAddr: socksAddr,
		cancel:    cancel,
		serverErr: serverErr,
		clientErr: clientErr,
		stopWait:  10 * time.Second,
	}, hub
}

// roomClientTraffic returns the number of server->client bytes delivered to the
// single client carrier of the given room.
func roomClientTraffic(hub *mpqPeerHub, room string) int64 {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for _, c := range hub.roomLocked(room).clients {
		return c.recvBytes.Load()
	}
	return 0
}

// TestMPQMultiRoomSeparateCalls proves the "separate calls" mode: a client's
// two paths live in two DIFFERENT rooms and the server hosts one carrier per
// room, so mpq bonds the two rooms into one session. Real SOCKS traffic must
// flow end-to-end AND traverse both rooms: a path whose room never moved bytes
// would mean the bond silently collapsed onto a single room.
func TestMPQMultiRoomSeparateCalls(t *testing.T) {
	echoAddr := startEchoServer(t)
	rt, hub := startMPQMultiRoomTunnel(t)
	defer rt.stop(t)

	// The client really opened one carrier per room (and the server one per
	// room) - the rooms are isolated domains, not one shared hub.
	hub.mu.Lock()
	nA := len(hub.roomLocked(testRoom + "-a").clients)
	nB := len(hub.roomLocked(testRoom + "-b").clients)
	huba := hub.roomLocked(testRoom+"-a").server != nil
	hubb := hub.roomLocked(testRoom+"-b").server != nil
	hub.mu.Unlock()
	if nA != 1 || nB != 1 {
		t.Fatalf("want one client carrier per room, got room-a=%d room-b=%d", nA, nB)
	}
	if !huba || !hubb {
		t.Fatalf("server carrier missing: room-a=%v room-b=%v", huba, hubb)
	}

	conn := connectViaSOCKS(t, rt.socksAddr, echoAddr)
	defer func() { _ = conn.Close() }()

	// A warm-up plus a bulk transfer: enough traffic for mpq's scheduler to
	// spread packets across both paths.
	echoRoundTrip(t, conn, 4096, 10*time.Second)
	echoRoundTrip(t, conn, 256*1024, 30*time.Second)

	// Both rooms' client carriers must have received bytes from the server,
	// proving outbound traffic was routed over both paths in the bond.
	if gotA := roomClientTraffic(hub, testRoom+"-a"); gotA == 0 {
		t.Fatal("room-a path carried no bytes (bond collapsed onto one room?)")
	}
	if gotB := roomClientTraffic(hub, testRoom+"-b"); gotB == 0 {
		t.Fatal("room-b path carried no bytes (bond collapsed onto one room?)")
	}
}
