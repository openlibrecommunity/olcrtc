package server

import (
	"context"
	"sync"
	"testing"

	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
)

type legacyControlRoutingStub struct {
	peerRoutingStub
	mu     sync.Mutex
	onData func([]byte)
}

func (l *legacyControlRoutingStub) ControlSend([]byte) error { return nil }
func (l *legacyControlRoutingStub) SetControlOnData(cb func([]byte)) {
	l.mu.Lock()
	l.onData = cb
	l.mu.Unlock()
}
func (l *legacyControlRoutingStub) ControlCanSend() bool { return true }

func reconnectTestServer(t *testing.T, link *peerRoutingStub) *Server {
	t.Helper()
	return &Server{
		baseCtx: context.Background(), ln: link, peerLn: link, ring: crypto.SingleEntry(newServerTestKeys(t), ""),
		health: runtime.NewHealthTracker(nil), onClose: func(string, string) {},
		peerSessions: make(map[string]*peerSession), peerStats: make(map[string]peerStat),
		done: make(chan struct{}),
	}
}

func assertNoBroadcastSession(t *testing.T, s *Server) {
	t.Helper()
	s.sessMu.RLock()
	defer s.sessMu.RUnlock()
	if s.pair != nil || s.conn != nil || s.session != nil {
		t.Fatalf("peer-routing reconnect installed broadcast data state: pair=%p conn=%p session=%p",
			s.pair, s.conn, s.session)
	}
	if len(s.peerSessions) != 0 {
		t.Fatalf("peer sessions after reconnect = %d, want 0", len(s.peerSessions))
	}
}

func TestPeerControlReconnectClearsPeersWithoutBroadcastPair(t *testing.T) {
	link := &peerControlRoutingStub{}
	s := reconnectTestServer(t, &link.peerRoutingStub)
	s.ln, s.peerLn = link, link
	oldSession, cleanup := mkServerSess(t)
	defer cleanup()
	peer := newPeerSession("peer-control", true, muxconn.PrePinned(newServerTestKeys(t), ""))
	peer.session = oldSession
	s.peerSessions[peer.peerID] = peer

	s.handleReconnect(context.Background())

	assertNoBroadcastSession(t, s)
	if !oldSession.IsClosed() {
		t.Fatal("peer data session remained open after reconnect")
	}
	link.mu.Lock()
	callbackInstalled := link.control != nil
	link.mu.Unlock()
	if !callbackInstalled {
		t.Fatal("per-peer control demultiplexer was not reinstalled")
	}
	s.closeSession()
}

func TestLegacyPeerReconnectReinstallsOnlySingletonControl(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	link := &legacyControlRoutingStub{}
	s := reconnectTestServer(t, &link.peerRoutingStub)
	s.baseCtx, s.ln, s.peerLn = ctx, link, link
	oldPeerSession, cleanupPeer := mkServerSess(t)
	defer cleanupPeer()
	oldControlSession, cleanupControl := mkServerSess(t)
	defer cleanupControl()
	peer := newPeerSession("peer-legacy", false, muxconn.PrePinned(newServerTestKeys(t), ""))
	peer.session = oldPeerSession
	s.peerSessions[peer.peerID] = peer
	s.controlSess = oldControlSession

	s.handleReconnect(ctx)

	assertNoBroadcastSession(t, s)
	if !oldPeerSession.IsClosed() || !oldControlSession.IsClosed() {
		t.Fatal("legacy peer or singleton control session remained open after reconnect")
	}
	s.sessMu.RLock()
	newControlConn, newControlSession := s.controlConn, s.controlSess
	s.sessMu.RUnlock()
	if newControlConn == nil || newControlSession == nil || newControlSession == oldControlSession {
		t.Fatal("singleton control session was not reinstalled")
	}
	link.mu.Lock()
	callbackInstalled := link.onData != nil
	link.mu.Unlock()
	if !callbackInstalled {
		t.Fatal("singleton control callback was not reinstalled")
	}
	cancel()
	s.closeSession()
}
