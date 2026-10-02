package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/handshake"
	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
)

type peerControlRoutingStub struct {
	peerRoutingStub
	mu      sync.Mutex
	control func(string, []byte)
}

func (p *peerControlRoutingStub) ControlSendTo(string, []byte) error { return nil }
func (p *peerControlRoutingStub) SetControlOnPeerData(cb func(string, []byte)) {
	p.mu.Lock()
	p.control = cb
	p.mu.Unlock()
}
func (p *peerControlRoutingStub) ControlPeerCanSend(string) bool { return true }

func newPeerLifecycleServer(t *testing.T) (*Server, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	link := &peerControlRoutingStub{}
	s := &Server{
		baseCtx: ctx, ln: link, peerLn: link, ring: crypto.SingleEntry(newServerTestKeys(t), ""),
		authHook: defaultAuthHook, onOpen: func(string, string, map[string]any) {},
		onClose: func(string, string) {}, health: runtime.NewHealthTracker(nil),
		peerSessions: make(map[string]*peerSession), peerStats: make(map[string]peerStat),
		done: make(chan struct{}),
	}
	cleanup := func() {
		cancel()
		s.closeSession()
		s.doneOnce.Do(func() { close(s.done) })
		s.wg.Wait()
	}
	return s, cleanup
}

func lookupPeer(s *Server, peerID string) *peerSession {
	s.sessMu.RLock()
	defer s.sessMu.RUnlock()
	return s.peerSessions[peerID]
}

func assertPeerPlanes(t *testing.T, peer *peerSession) {
	t.Helper()
	if peer == nil || peer.sessionReady == nil || peer.dataConn() == nil || peer.dataSession() == nil {
		t.Fatal("peer data plane or readiness signal is missing")
	}
	if conn, session := peer.controlPlane(); conn == nil || session == nil {
		t.Fatal("peer control plane is missing")
	}
}

func TestPeerPlaneAttachmentOrder(t *testing.T) {
	tests := []struct {
		name  string
		first func(*Server, string, []byte)
		last  func(*Server, string, []byte)
	}{
		{"control-first", (*Server).onPeerControlData, (*Server).onPeerData},
		{"data-first", (*Server).onPeerData, (*Server).onPeerControlData},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, cleanup := newPeerLifecycleServer(t)
			defer cleanup()
			test.first(s, "peer-order", nil)
			test.last(s, "peer-order", nil)
			assertPeerPlanes(t, lookupPeer(s, "peer-order"))
		})
	}
}

func TestPeerPlaneAttachmentConcurrent(t *testing.T) {
	s, cleanup := newPeerLifecycleServer(t)
	defer cleanup()

	const callbacks = 32
	var wg sync.WaitGroup
	for i := range callbacks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				s.onPeerData("peer-concurrent", nil)
				return
			}
			s.onPeerControlData("peer-concurrent", nil)
		}()
	}
	wg.Wait()

	assertPeerPlanes(t, lookupPeer(s, "peer-concurrent"))
	s.sessMu.RLock()
	count := len(s.peerSessions)
	s.sessMu.RUnlock()
	if count != 1 {
		t.Fatalf("peer session count = %d, want 1", count)
	}
}

func TestRejectedPeerHandshakeWakesServePeer(t *testing.T) {
	dataServer, _, cleanupData := smuxPair(t)
	defer cleanupData()
	controlServer, controlClient, cleanupControl := smuxPair(t)
	defer cleanupControl()

	peer := newPeerSession("peer-reject", true, muxconn.PrePinned(newServerTestKeys(t), ""))
	peer.session = dataServer
	peer.controlSess = controlServer
	s := &Server{
		ln: &serverLinkStub{}, authHook: func(string, map[string]any) (string, error) {
			return "", errors.New("rejected")
		},
		health: runtime.NewHealthTracker(nil), peerSessions: map[string]*peerSession{peer.peerID: peer},
		peerStats: make(map[string]peerStat), done: make(chan struct{}),
	}
	serveDone := make(chan struct{})
	go func() {
		s.servePeer(peer)
		close(serveDone)
	}()
	acceptDone := make(chan struct{})
	go func() {
		s.acceptPeerHandshake(context.Background(), peer)
		close(acceptDone)
	}()

	stream, err := controlClient.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	_, _, err = handshake.Client(stream, "rejected-device", nil)
	if !errors.Is(err, handshake.ErrRejected) {
		t.Fatalf("Client() error = %v, want ErrRejected", err)
	}

	for name, done := range map[string]<-chan struct{}{"servePeer": serveDone, "acceptPeerHandshake": acceptDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s did not exit after handshake rejection", name)
		}
	}
	select {
	case <-s.done:
		t.Fatal("peer handshake failure required global server shutdown")
	default:
	}
}

// TestPeerAdmissionIsBounded locks in the cap on per-peer state. Peer IDs
// arrive from the transport before anything is decrypted, so any participant
// in the room can mint them; each one used to buy a muxconn, an smux session
// and two goroutines that nothing reclaimed.
func TestPeerAdmissionIsBounded(t *testing.T) {
	s, cleanup := newPeerLifecycleServer(t)
	defer cleanup()

	for i := range maxPeerSessions + 32 {
		s.onPeerData(fmt.Sprintf("%08x", i+1), []byte("junk"))
	}

	s.sessMu.RLock()
	admitted := len(s.peerSessions)
	s.sessMu.RUnlock()

	if admitted > maxPeerSessions {
		t.Fatalf("admitted %d peers, want at most %d", admitted, maxPeerSessions)
	}
}

// TestPeerAdmissionRefusedWhileStopping locks in that a frame arriving during
// teardown does not rebuild peer state into the map shutdown has already
// swapped out, leaving a goroutine that outlives wg.Wait.
func TestPeerAdmissionRefusedWhileStopping(t *testing.T) {
	s, cleanup := newPeerLifecycleServer(t)
	defer cleanup()

	s.doneOnce.Do(func() { close(s.done) })
	s.onPeerData("0000dead", []byte("junk"))

	if peer := lookupPeer(s, "0000dead"); peer != nil {
		t.Fatal("peer session created while the server was stopping")
	}
}
