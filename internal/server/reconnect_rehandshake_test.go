package server

import (
	"context"
	"net"
	"testing"

	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/transport"

	"github.com/xtaci/smux"

	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
	"github.com/openlibrecommunity/olcrtc/internal/tunnelcore"
)

// mkServerSess builds a server-side smux session over one end of a pipe.
// The far end is closed by the returned cleanup.
func mkServerSess(t *testing.T) (*smux.Session, func()) {
	t.Helper()
	a, b := net.Pipe()
	sess, err := smux.Server(a, testSmuxCfg())
	if err != nil {
		_ = a.Close()
		_ = b.Close()
		t.Fatalf("smux.Server() error = %v", err)
	}
	return sess, func() {
		_ = sess.Close()
		_ = a.Close()
		_ = b.Close()
	}
}

// TestSwapSessionDiscardsStaleReinstall confirms the guard still rejects a
// reinstall whose dying session matches neither the live data nor control
// session (another reinstall already won the race).
func TestSwapSessionDiscardsStaleReinstall(t *testing.T) {
	keys := newServerTestKeys(t)
	liveData, cleanupL := mkServerSess(t)
	defer cleanupL()
	stale, cleanupS := mkServerSess(t)
	defer cleanupS()
	newData, cleanupND := mkServerSess(t)
	defer cleanupND()

	ln := &peerRoutingStub{}
	s := &Server{
		ln:      ln,
		ring:    crypto.SingleEntry(keys, ""),
		session: liveData,
		health:  runtime.NewHealthTracker(nil),
	}
	r := &tunnelcore.SessionPair{DataSession: newData, DataConn: muxconn.New(ln, keys)}
	if ok := s.swapSession(stale, r, r.DataConn.Group()); ok {
		t.Fatal("swapSession accepted a stale reinstall that matched no live session")
	}
	s.sessMu.RLock()
	got := s.session
	s.sessMu.RUnlock()
	if got != liveData {
		t.Fatalf("live session was clobbered by a stale reinstall: got %p want %p", got, liveData)
	}
}

// peerRoutingStub is a transport stub that satisfies PeerTransport so the
// server treats it as peer-routing capable.
type peerRoutingStub struct {
	closed bool
}

func (p *peerRoutingStub) Connect(context.Context) error   { return nil }
func (p *peerRoutingStub) Send([]byte) error               { return nil }
func (p *peerRoutingStub) Close() error                    { p.closed = true; return nil }
func (p *peerRoutingStub) SetReconnectCallback(func())     {}
func (p *peerRoutingStub) SetShouldReconnect(func() bool)  {}
func (p *peerRoutingStub) SetEndedCallback(func(string))   {}
func (p *peerRoutingStub) WatchConnection(context.Context) {}
func (p *peerRoutingStub) CanSend() bool                   { return true }
func (p *peerRoutingStub) Features() transport.Features    { return transport.Features{} }
func (p *peerRoutingStub) Reconnect(string)                {}
func (p *peerRoutingStub) SendTo(string, []byte) error     { return nil }
func (p *peerRoutingStub) SupportsPeerRouting() bool       { return true }
