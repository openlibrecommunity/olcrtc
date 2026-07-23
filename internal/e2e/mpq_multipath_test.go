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
// several client carriers and one server carrier share a room; the server
// distinguishes each client by a stable peerID. It is the substrate the mpq
// server demuxes over (mpqx.ServerPacketConn). It is deliberately separate from
// the broadcast memoryStream used by the legacy tests: multipath REQUIRES
// per-peer routing (a broadcast carrier would cross-feed the paths' QUIC conns).
//
// A carrier registered with OnPeerData set is the server end; one registered
// with only OnData set is a client end and is assigned a unique peerID. A client
// Send arrives at the server as OnPeerData(peerID, data); the server's
// SendTo(peerID, data) is delivered back to that one client's OnData.

type mpqPeerHub struct {
	mu       sync.Mutex
	server   *mpqPeerStream
	clients  map[string]*mpqPeerStream
	nextID   atomic.Int64
	serverUp chan struct{} // closed once the server end is connected
}

func newMPQPeerHub() *mpqPeerHub {
	return &mpqPeerHub{
		clients:  make(map[string]*mpqPeerStream),
		serverUp: make(chan struct{}),
	}
}

type mpqPeerStream struct {
	hub        *mpqPeerHub
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
}

func (s *mpqPeerStream) Connect(context.Context) error {
	s.mu.Lock()
	s.connected = true
	s.mu.Unlock()
	if s.isServer {
		s.hub.mu.Lock()
		s.hub.server = s
		serverUp := s.hub.serverUp
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
	srv := s.hub.server
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
	c := s.hub.clients[peerID]
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
	cb(append([]byte(nil), data...))
	return nil
}

// WaitForPeer (engine.PeerReadySession): a client waits until the server end is
// up. The server end returns immediately.
func (s *mpqPeerStream) WaitForPeer(ctx context.Context) error {
	if s.isServer {
		return nil
	}
	s.hub.mu.Lock()
	serverUp := s.hub.serverUp
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

func (s *mpqPeerStream) SetReconnectCallback(func(*webrtc.DataChannel)) {}
func (s *mpqPeerStream) SetShouldReconnect(func() bool)                  {}
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

// registerMPQPeerCarrier registers a peer-routing carrier engine and returns its
// name plus the hub, so the test can reach individual client paths (e.g. to kill
// one).
func registerMPQPeerCarrier(t *testing.T) (string, *mpqPeerHub) {
	t.Helper()
	session.RegisterDefaults()
	hub := newMPQPeerHub()
	name := "e2e-mpq-peer-" + t.Name()
	enginebuiltin.Register(name, func(_ context.Context, cfg enginebuiltin.Config) (engine.Session, error) {
		s := &mpqPeerStream{
			hub:        hub,
			onData:     cfg.OnData,
			onPeerData: cfg.OnPeerData,
		}
		if cfg.OnPeerData != nil {
			s.isServer = true
		} else {
			s.peerID = fmt.Sprintf("mpq-client-%d", hub.nextID.Add(1))
			hub.mu.Lock()
			hub.clients[s.peerID] = s
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
	select {
	case <-hub.serverUp:
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
	nClients := len(hub.clients)
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

// TestMPQMultipathSurvivesPathDrop kills one of the two paths mid-tunnel and
// verifies data still flows over the surviving path (mpq's reliable bonding
// retransmits the lost chunks on the healthy path). Best-effort: uses a generous
// budget because loss recovery on the dead path adds RTO latency.
func TestMPQMultipathSurvivesPathDrop(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: waits out the dead path's QUIC idle timeout (~45s)")
	}
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
	for _, c := range hub.clients {
		victim = c
		break
	}
	hub.mu.Unlock()
	if victim == nil {
		t.Fatal("no client path to kill")
	}
	victim.kill()
	t.Logf("killed path peerID=%s", victim.peerID)

	// Data must still round-trip via the surviving path. Generous budget: until
	// the dead path hits its QUIC idle timeout (~45s) the scheduler keeps trying
	// it and relies on RTO retransmission onto the healthy path.
	echoRoundTrip(t, conn, 64*1024, 90*time.Second)
}
