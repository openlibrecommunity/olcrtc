package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xtaci/smux"

	"github.com/openlibrecommunity/olcrtc/internal/framing"
	"github.com/openlibrecommunity/olcrtc/internal/handshake"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/openlibrecommunity/olcrtc/internal/tunnelcore"
)

const (
	// maxPeerSessions bounds how many per-peer stacks the server holds at
	// once. Peer IDs come from the transport before anything is decrypted,
	// so any participant in the room can mint them: each new ID otherwise
	// costs a muxconn, an smux session and two goroutines that nothing
	// reclaims until a handshake that never comes.
	maxPeerSessions = 128

	// peerHandshakeTimeout bounds how long a peer session may sit waiting
	// for its handshake before it is released. A peer that reappears simply
	// gets a fresh session built on its next frame.
	peerHandshakeTimeout = 4 * handshake.DefaultTimeout

	// peerLimitWarnInterval rate-limits the peer-cap warning so a flood of
	// bogus peer IDs cannot turn the log into the outage.
	peerLimitWarnInterval = time.Minute
)

type peerStat struct {
	deviceID string
	openedAt time.Time
}

// peerSession holds one client's independently synchronized peer-routing state.
type peerSession struct {
	peerID        string
	sessionReady  chan struct{}
	readyOnce     sync.Once
	handshakeOnce sync.Once
	closeOnce     sync.Once
	mu            sync.Mutex
	conn          *muxconn.Conn
	session       *smux.Session
	controlConn   *muxconn.Conn
	controlSess   *smux.Session
	controlStrm   *smux.Stream
	controlStop   context.CancelFunc
	sessionID     string
	deviceID      string
	closed        bool
}

func newPeerSession(peerID string, needsControl bool) *peerSession {
	peer := &peerSession{peerID: peerID}
	if needsControl {
		peer.sessionReady = make(chan struct{})
	}
	return peer
}

func (ps *peerSession) signalReady() {
	if ps.sessionReady != nil {
		ps.readyOnce.Do(func() { close(ps.sessionReady) })
	}
}

func (ps *peerSession) startHandshake(start func()) {
	ps.handshakeOnce.Do(start)
}

func (ps *peerSession) sid() string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.sessionID
}

func (ps *peerSession) setHandshake(result handshakeResult) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.closed {
		return false
	}
	ps.sessionID = result.sessionID
	ps.deviceID = result.deviceID
	return true
}

func (ps *peerSession) attachData(conn *muxconn.Conn, session *smux.Session) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.closed || ps.conn != nil || ps.session != nil {
		return false
	}
	ps.conn = conn
	ps.session = session
	return true
}

func (ps *peerSession) dataConn() *muxconn.Conn {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.conn
}

func (ps *peerSession) dataSession() *smux.Session {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.session
}

func (ps *peerSession) controlPlane() (*muxconn.Conn, *smux.Session) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.controlConn, ps.controlSess
}

func (ps *peerSession) attachControl(conn *muxconn.Conn, session *smux.Session) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.closed || ps.controlConn != nil || ps.controlSess != nil {
		return false
	}
	ps.controlConn = conn
	ps.controlSess = session
	return true
}

func (ps *peerSession) setControl(stream *smux.Stream, stop context.CancelFunc) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.closed {
		return false
	}
	ps.controlStrm = stream
	ps.controlStop = stop
	return true
}

type teardown struct {
	conn        *muxconn.Conn
	session     *smux.Session
	controlConn *muxconn.Conn
	controlSess *smux.Session
	controlStrm *smux.Stream
	controlStop context.CancelFunc
	sessionID   string
}

func (ps *peerSession) closeSnapshot() teardown {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.closed = true
	return teardown{
		conn: ps.conn, session: ps.session, controlConn: ps.controlConn,
		controlSess: ps.controlSess, controlStrm: ps.controlStrm,
		controlStop: ps.controlStop, sessionID: ps.sessionID,
	}
}

func (s *Server) installPeerControlPlane(control transport.PeerControlPlane) {
	control.SetControlOnPeerData(s.onPeerControlData)
}

func (s *Server) onPeerControlData(peerID string, data []byte) {
	peer := s.getOrCreatePeerControlSession(peerID)
	if peer == nil {
		return
	}
	if conn, _ := peer.controlPlane(); conn != nil {
		conn.Push(data)
	}
}

func (s *Server) getOrCreatePeerControlSession(peerID string) *peerSession {
	if peerID == "" {
		return nil
	}
	_, supportsControl := s.ln.(transport.PeerControlPlane)
	if !supportsControl {
		return nil
	}
	s.sessMu.Lock()
	peer := s.peerSessions[peerID]
	if peer != nil {
		if conn, _ := peer.controlPlane(); conn != nil {
			s.sessMu.Unlock()
			return peer
		}
	} else {
		if !s.mayAdmitPeerLocked(nil) {
			s.sessMu.Unlock()
			return nil
		}
		peer = newPeerSession(peerID, true)
	}
	conn := muxconn.NewPeerControlUnbound(s.ln, s.keys, peerID)
	if conn == nil {
		s.sessMu.Unlock()
		return nil
	}
	session, err := tunnelcore.NewSession(
		conn, tunnelcore.ServerRole, runtime.ControlSmuxConfig(runtime.MaxPayload(s.ln)),
	)
	if err != nil {
		logger.Warnf("control smux init failed for peer %s: %v", peerID, err)
		_ = conn.Close()
		s.sessMu.Unlock()
		return nil
	}
	if !peer.attachControl(conn, session) {
		_ = session.Close()
		_ = conn.Close()
		s.sessMu.Unlock()
		return peer
	}
	s.peerSessions[peerID] = peer
	s.sessMu.Unlock()
	logger.Infof("server: peer control session created peerID=%s", peerID)
	peer.startHandshake(func() {
		s.goTracked(func() { s.acceptPeerHandshake(s.streamContext(), peer) })
		s.goTracked(func() { s.expirePeerHandshake(peer) })
	})
	return peer
}

// expirePeerHandshake releases a peer whose handshake never lands. A
// control-only peer never reaches servePeer, so nothing else bounds it: its
// accept goroutine simply blocks in AcceptStream, and enough of them fill the
// admission cap and lock legitimate peers out.
func (s *Server) expirePeerHandshake(peer *peerSession) {
	timer := time.NewTimer(peerHandshakeTimeout)
	defer timer.Stop()
	select {
	case <-peer.sessionReady:
	case <-s.done:
	case <-timer.C:
		if peer.sid() != "" {
			return
		}
		logger.Infof("server: peer %s did not handshake within %s - releasing control session",
			peer.peerID, peerHandshakeTimeout)
		s.removePeer(peer, "handshake timeout")
	}
}

// mayAdmitPeerLocked reports whether a new per-peer stack may be built.
// Callers hold sessMu, the same lock closeSession takes to swap the peer map
// out, so a peer admitted here is always one teardown will see - and a peer
// arriving after teardown is refused instead of leaking a goroutine that
// outlives wg.Wait.
func (s *Server) mayAdmitPeerLocked(existing *peerSession) bool {
	if s.stopping() {
		return false
	}
	if existing != nil {
		return true
	}
	if len(s.peerSessions) >= maxPeerSessions {
		s.warnPeerLimit()
		return false
	}
	return true
}

func (s *Server) warnPeerLimit() {
	now := time.Now().UnixNano()
	last := s.peerLimitWarn.Load()
	if now-last < int64(peerLimitWarnInterval) {
		return
	}
	if !s.peerLimitWarn.CompareAndSwap(last, now) {
		return
	}
	logger.Warnf("server: peer session limit %d reached - refusing new peers", maxPeerSessions)
}

// goTracked runs fn on a goroutine shutdown waits for. Registration happens
// under sessMu, which shutdown also takes before wg.Wait, so wg.Add can never
// race a Wait that has already started.
func (s *Server) goTracked(fn func()) {
	s.sessMu.Lock()
	if s.stopping() {
		s.sessMu.Unlock()
		return
	}
	s.wg.Add(1)
	s.sessMu.Unlock()
	go func() {
		defer s.wg.Done()
		fn()
	}()
}

func (s *Server) onPeerData(peerID string, data []byte) {
	peer := s.getPeerSession(peerID)
	if peer == nil {
		s.onData(data)
		return
	}
	tunnelcore.PushData(peer.dataConn(), data)
}

func (s *Server) getPeerSession(peerID string) *peerSession {
	if peerID == "" || s.peerLn == nil {
		return nil
	}
	s.sessMu.Lock()
	peer := s.peerSessions[peerID]
	if peer != nil && peer.dataConn() != nil {
		s.sessMu.Unlock()
		return peer
	}
	if !s.mayAdmitPeerLocked(peer) {
		s.sessMu.Unlock()
		return nil
	}
	conn := muxconn.NewPeer(s.peerLn, s.keys, peerID)
	session, err := tunnelcore.NewSession(conn, tunnelcore.ServerRole, runtime.SmuxConfigFor(s.ln))
	if err != nil {
		s.sessMu.Unlock()
		logger.Warnf("smux server init failed for peer %s: %v", peerID, err)
		_ = conn.Close()
		return nil
	}
	if peer == nil {
		_, needsControl := s.ln.(transport.PeerControlPlane)
		peer = newPeerSession(peerID, needsControl)
		s.peerSessions[peerID] = peer
	}
	if !peer.attachData(conn, session) {
		_ = session.Close()
		_ = conn.Close()
		s.sessMu.Unlock()
		return peer
	}
	s.sessMu.Unlock()
	s.goTracked(func() { s.servePeer(peer) })
	return peer
}

func (s *Server) acceptPeerHandshake(ctx context.Context, peer *peerSession) {
	const maxStaleRetries = 3
	_, session := peer.controlPlane()
	if session == nil {
		return
	}
	for retry := 0; retry <= maxStaleRetries; retry++ {
		stream, err := session.AcceptStream()
		if err != nil {
			if ctx.Err() == nil {
				logger.Infof("server: AcceptStream(peer control=%s) error: %v", peer.peerID, err)
				s.removePeer(peer, "handshake failed")
			}
			return
		}
		_ = stream.SetDeadline(time.Now().Add(handshake.DefaultTimeout))
		hello, sessionID, err := handshake.Server(stream, s.authHook, s.localPeerID())
		_ = stream.SetDeadline(time.Time{})
		if err != nil {
			_ = stream.Close()
			if errors.Is(err, framing.ErrFrameTooLarge) && retry < maxStaleRetries {
				logger.Debugf("handshake peer=%s: stale stream retry %d: %v", peer.peerID, retry+1, err)
				continue
			}
			logger.Warnf("handshake peer=%s failed: %v", peer.peerID, err)
			s.removePeer(peer, "handshake failed")
			return
		}
		if !peer.setHandshake(handshakeResult{sessionID: sessionID, deviceID: hello.DeviceID}) {
			_ = stream.Close()
			return
		}
		peer.signalReady()
		s.health.RecordSession(sessionID)
		s.onOpen(sessionID, hello.DeviceID, hello.Claims)
		s.trackPeerOpen(sessionID, hello.DeviceID)
		logger.Infof("peer session %s opened (peer=%s device=%s)", sessionID, peer.peerID, hello.DeviceID)
		s.startPeerControlLoop(ctx, peer, stream)
		return
	}
}

func (s *Server) startPeerControlLoop(ctx context.Context, peer *peerSession, stream *smux.Stream) {
	controlCtx, stop := context.WithCancel(ctx)
	if !peer.setControl(stream, stop) {
		stop()
		_ = stream.Close()
		return
	}
	runner := tunnelcore.ControlRunner{
		Transport: s.ln, Config: s.liveness, Health: s.health,
		LogFields: func() string { return "role=server peer=" + peer.peerID },
		OnDeath:   func(error) { s.removePeer(peer, "liveness") },
	}
	s.goTracked(func() {
		defer func() {
			// A cancelled control context means teardown is already under way,
			// and closePeerSession owns this stream from that point: it still
			// has to write the graceful close onto it before closing it itself.
			// Closing here first turned that write into io.ErrClosedPipe, so the
			// peer was never told we were leaving and had to discover the
			// retired room the slow way, through missed liveness pongs.
			if controlCtx.Err() == nil {
				_ = stream.Close()
			}
		}()
		runner.Run(controlCtx, stream)
	})
}

func (s *Server) servePeer(peer *peerSession) {
	if peer.sid() == "" && !s.establishPeerSession(peer) {
		return
	}
	session := peer.dataSession()
	if session == nil {
		return
	}
	ctx := s.streamContext()
	sessionID := peer.sid()
	for {
		if s.stopping() {
			return
		}
		stream, err := session.AcceptStream()
		if err != nil {
			if !s.stopping() {
				logger.Infof("server: AcceptStream(peer=%s) error - closing peer session: %v", peer.peerID, err)
				s.removePeer(peer, "closed")
			}
			return
		}
		s.goTracked(func() { s.handleStream(ctx, stream, sessionID) })
	}
}

func (s *Server) streamContext() context.Context {
	if s.baseCtx != nil {
		return s.baseCtx
	}
	return context.Background()
}

func (s *Server) establishPeerSession(peer *peerSession) bool {
	if peer.sessionReady != nil {
		return s.waitPeerHandshake(peer)
	}
	session := peer.dataSession()
	if session == nil {
		return false
	}
	ctx := s.streamContext()
	stream, result, ok := s.acceptHandshake(ctx, session)
	if !ok {
		s.removePeer(peer, "handshake failed")
		return false
	}
	if !peer.setHandshake(result) {
		_ = stream.Close()
		return false
	}
	s.startPeerControlLoop(ctx, peer, stream)
	return true
}

// waitPeerHandshake blocks until the peer's control-plane handshake lands.
// The wait is bounded: an unauthenticated peer that never handshakes would
// otherwise pin its whole session stack until the server stops, and a peer
// that comes back simply gets a fresh session built on its next frame.
func (s *Server) waitPeerHandshake(peer *peerSession) bool {
	if peer.sessionReady == nil {
		return false
	}
	timer := time.NewTimer(peerHandshakeTimeout)
	defer timer.Stop()
	select {
	case <-peer.sessionReady:
		return peer.sid() != ""
	case <-timer.C:
		logger.Infof("server: peer %s did not handshake within %s - releasing session",
			peer.peerID, peerHandshakeTimeout)
		s.removePeer(peer, "handshake timeout")
		return false
	case <-s.done:
		s.removePeer(peer, "closed")
		return false
	}
}

func (s *Server) removePeer(peer *peerSession, reason string) {
	if peer == nil {
		return
	}
	s.sessMu.Lock()
	current := s.peerSessions[peer.peerID] == peer
	if current {
		delete(s.peerSessions, peer.peerID)
	}
	s.sessMu.Unlock()
	if current {
		s.closePeerSession(peer, reason)
	}
}

func (s *Server) closePeerSession(peer *peerSession, reason string) {
	peer.closeOnce.Do(func() {
		teardown := peer.closeSnapshot()
		peer.signalReady()
		// Logged either way: on a rotation this frame is what moves the client
		// onto its warm standby at once, and "was it even sent?" is otherwise
		// invisible from the server side.
		if teardown.controlStrm == nil {
			logger.Infof("peer close: device=%s reason=%s no control stream to notify on", peer.deviceID, reason)
		} else {
			logger.Infof("peer close: device=%s reason=%s - sending control close", peer.deviceID, reason)
		}
		tunnelcore.NotifyControlClose(teardown.controlStrm)
		if teardown.controlStop != nil {
			teardown.controlStop()
		}
		if teardown.controlStrm != nil {
			_ = teardown.controlStrm.Close()
		}
		if teardown.controlSess != nil {
			_ = teardown.controlSess.Close()
		}
		if teardown.controlConn != nil {
			_ = teardown.controlConn.Close()
		}
		if teardown.session != nil {
			_ = teardown.session.Close()
		}
		if teardown.conn != nil {
			_ = teardown.conn.Close()
		}
		if teardown.sessionID != "" {
			s.onClose(teardown.sessionID, reason)
			s.trackPeerClose(teardown.sessionID, reason)
		}
	})
}

func (s *Server) trackPeerOpen(sessionID, deviceID string) {
	s.peersMu.Lock()
	s.peerStats[sessionID] = peerStat{deviceID: deviceID, openedAt: time.Now()}
	line := s.peersLineLocked()
	s.peersMu.Unlock()
	logger.Infof("peer connected: device=%s session=%s", deviceID, sessionID)
	logger.Infof("%s", line)
}

func (s *Server) trackPeerClose(sessionID, reason string) {
	s.peersMu.Lock()
	stat, ok := s.peerStats[sessionID]
	if !ok {
		s.peersMu.Unlock()
		return
	}
	delete(s.peerStats, sessionID)
	line := s.peersLineLocked()
	s.peersMu.Unlock()
	logger.Infof("peer disconnected: device=%s session=%s reason=%s duration=%s",
		stat.deviceID, sessionID, reason, time.Since(stat.openedAt).Round(time.Second))
	logger.Infof("%s", line)
}

func (s *Server) peersLineLocked() string {
	devices := make([]string, 0, len(s.peerStats))
	for _, stat := range s.peerStats {
		devices = append(devices, stat.deviceID)
	}
	sort.Strings(devices)
	return fmt.Sprintf("Current peers count: %d, Devices: [%s]", len(s.peerStats), strings.Join(devices, ", "))
}

func (s *Server) logPeersLine() {
	s.peersMu.Lock()
	line := s.peersLineLocked()
	s.peersMu.Unlock()
	logger.Infof("%s", line)
}

func (s *Server) stopping() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}
