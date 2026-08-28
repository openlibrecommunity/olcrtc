package server

import (
	"context"
	"fmt"

	"github.com/xtaci/smux"

	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/openlibrecommunity/olcrtc/internal/tunnelcore"
)

func (s *Server) bringUpLink(ctx context.Context, cfg Config, cancel context.CancelFunc) error {
	s.baseCtx = ctx
	linkCfg := tunnelcore.BuildTransportConfig(tunnelcore.LinkConfig{
		Provider: cfg.Provider, RoomURL: cfg.RoomURL, Engine: cfg.Engine,
		URL: cfg.URL, Token: cfg.Token, ProviderToken: cfg.ProviderToken,
		ChannelID: cfg.ChannelID, DNSServer: s.dnsServer,
		Options: cfg.TransportOptions, Traffic: cfg.Traffic,
	}, tunnelcore.LinkRoleConfig{
		OnData: s.onData, OnPeerData: s.onPeerData, Resolver: s.resolver,
		ProxyAddr: s.socksProxyAddr, ProxyPort: s.socksProxyPort,
	})
	ln, err := transport.New(ctx, cfg.Transport, linkCfg)
	if err != nil {
		return fmt.Errorf("failed to create transport: %w", err)
	}
	s.ln = ln
	if peerLn, ok := ln.(transport.PeerTransport); ok && peerLn.SupportsPeerRouting() {
		s.peerLn = peerLn
	}
	ln.SetEndedCallback(func(reason string) {
		logger.Infof("Server link reported conference end: %s", reason)
		cancel()
	})
	ln.SetShouldReconnect(func() bool { return ctx.Err() == nil })
	ln.SetReconnectCallback(func() {
		if ctx.Err() == nil {
			s.handleReconnect(ctx)
		}
	})
	logger.Infof("Connecting transport=%s provider=%s ...", cfg.Transport, cfg.Provider)
	if s.peerLn == nil {
		s.installSession()
	} else {
		s.installControlSession(ctx)
	}
	if err := ln.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect link: %w", err)
	}
	logger.Infof("Link connected")
	s.logPeersLine()
	s.goTracked(func() { ln.WatchConnection(ctx) })
	return nil
}

func (s *Server) installSession() {
	pair, err := tunnelcore.NewSessionPair(s.ln, s.keys, tunnelcore.ServerRole)
	if pair == nil {
		logger.Warnf("smux server init failed: %v", err)
		return
	}
	if err != nil {
		logger.Warnf("control smux server init failed: %v", err)
	}
	s.sessMu.Lock()
	s.installPairLocked(pair)
	s.sessMu.Unlock()
	s.state.broadcast()
	if pair.HasIsolatedControl() {
		control := pair.ControlSession
		s.goTracked(func() { s.acceptSingletonHandshake(s.baseCtx, control) })
	}
}

func (s *Server) installControlSession(ctx context.Context) {
	if peerControl, ok := s.ln.(transport.PeerControlPlane); ok {
		s.installPeerControlPlane(peerControl)
		return
	}
	conn, session, err := tunnelcore.NewControlSession(s.ln, s.keys, tunnelcore.ServerRole)
	if err != nil {
		logger.Warnf("control smux server init failed (peer-routing): %v", err)
		return
	}
	if conn == nil {
		return
	}
	s.sessMu.Lock()
	s.controlConn = conn
	s.controlSess = session
	s.sessMu.Unlock()
	s.goTracked(func() { s.acceptSingletonHandshake(ctx, session) })
}

func (s *Server) handleReconnect(ctx context.Context) {
	s.health.RecordReconnect()
	logger.Infof("server reconnect reason=provider - tearing down smux session")
	if s.peerLn != nil {
		s.reinstallPeerRouting(ctx)
		return
	}
	s.sessMu.RLock()
	current := s.session
	s.sessMu.RUnlock()
	s.reinstallSession(ctx, current)
}

func (s *Server) reinstallSession(ctx context.Context, dead *smux.Session) {
	if s.peerLn != nil {
		s.reinstallPeerRouting(ctx)
		return
	}
	s.reinstallMu.Lock()
	defer s.reinstallMu.Unlock()
	s.sessMu.RLock()
	if s.pair != nil {
		// ai-generated: changed s.pair.CloseConns() to s.pair.Close() and
		// added this comment explaining why.
		// Close (not CloseConns): the stale pair's smux Sessions must be
		// closed too, not just their muxconns - otherwise a recvLoop
		// currently parked on its flow-control bucket (waiting on
		// bucketNotify/die, not reading conn) never notices the conn died
		// and AcceptStream on it stays blocked until something else
		// eventually pokes it.
		_ = s.pair.Close()
	} else {
		if s.conn != nil {
			_ = s.conn.Close()
		}
		if s.controlConn != nil {
			_ = s.controlConn.Close()
		}
	}
	s.sessMu.RUnlock()
	replacement, err := tunnelcore.NewSessionPair(s.ln, s.keys, tunnelcore.ServerRole)
	if replacement == nil {
		logger.Warnf("smux server re-init failed: %v", err)
		return
	}
	if err != nil {
		logger.Warnf("control smux server re-init failed: %v", err)
	}
	if !s.swapSession(dead, replacement) {
		return
	}
	if replacement.HasIsolatedControl() {
		control := replacement.ControlSession
		s.goTracked(func() { s.acceptSingletonHandshake(ctx, control) })
	}
}

type peerRoutingTeardown struct {
	pair           *tunnelcore.SessionPair
	conn           *muxconn.Conn
	session        *smux.Session
	controlConn    *muxconn.Conn
	controlSession *smux.Session
	controlStream  *smux.Stream
	controlStop    context.CancelFunc
	peers          map[string]*peerSession
	sessionID      string
}

func (s *Server) reinstallPeerRouting(ctx context.Context) {
	s.reinstallMu.Lock()
	defer s.reinstallMu.Unlock()
	teardown := s.detachPeerRouting()
	s.closePeerRouting(teardown)
	s.installControlSession(ctx)
}

func (s *Server) detachPeerRouting() peerRoutingTeardown {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	teardown := peerRoutingTeardown{
		pair: s.pair, conn: s.conn, session: s.session,
		controlConn: s.controlConn, controlSession: s.controlSess,
		controlStream: s.controlStrm, controlStop: s.controlStop,
		peers: s.peerSessions, sessionID: s.sessionID,
	}
	s.peerSessions = make(map[string]*peerSession)
	s.pair, s.conn, s.session = nil, nil, nil
	s.controlConn, s.controlSess = nil, nil
	s.controlStrm, s.controlStop = nil, nil
	s.sessionID, s.deviceID = "", ""
	return teardown
}

func (s *Server) closePeerRouting(teardown peerRoutingTeardown) {
	tunnelcore.NotifyControlClose(teardown.controlStream)
	if teardown.controlStop != nil {
		teardown.controlStop()
	}
	if teardown.controlStream != nil {
		_ = teardown.controlStream.Close()
	}
	closeServerPair(teardown.pair, teardown.session, teardown.controlSession)
	if teardown.pair == nil {
		if teardown.conn != nil {
			_ = teardown.conn.Close()
		}
		if teardown.controlConn != nil {
			_ = teardown.controlConn.Close()
		}
	}
	if teardown.sessionID != "" {
		s.onClose(teardown.sessionID, "reconnect")
		s.trackPeerClose(teardown.sessionID, "reconnect")
	}
	for _, peer := range teardown.peers {
		s.closePeerSession(peer, "reconnect")
	}
}

func (s *Server) staleReinstall(dead *smux.Session) bool {
	return dead != nil && dead != s.session && dead != s.controlSess
}

func (s *Server) swapSession(dead *smux.Session, replacement *tunnelcore.SessionPair) bool {
	s.sessMu.Lock()
	if s.peerLn != nil {
		s.sessMu.Unlock()
		_ = replacement.Close()
		return false
	}
	if s.staleReinstall(dead) {
		s.sessMu.Unlock()
		_ = replacement.Close()
		return false
	}
	oldPair := s.pair
	oldSession := s.session
	oldControlSession := s.controlSess
	oldControl := s.controlStrm
	oldControlStop := s.controlStop
	oldSessionID := s.sessionID
	s.installPairLocked(replacement)
	s.controlStrm = nil
	s.controlStop = nil
	s.sessionID = ""
	s.deviceID = ""
	s.sessMu.Unlock()
	s.state.broadcast()
	if oldControlStop != nil {
		oldControlStop()
	}
	closeServerPair(oldPair, oldSession, oldControlSession)
	if oldControl != nil {
		_ = oldControl.Close()
	}
	if oldSessionID != "" {
		s.onClose(oldSessionID, "reconnect")
		s.trackPeerClose(oldSessionID, "reconnect")
	}
	return true
}

func closeServerPair(pair *tunnelcore.SessionPair, session, controlSession *smux.Session) {
	if pair != nil {
		_ = pair.Close()
		return
	}
	if session != nil {
		_ = session.Close()
	}
	if controlSession != nil && controlSession != session {
		_ = controlSession.Close()
	}
}

func (s *Server) installPairLocked(pair *tunnelcore.SessionPair) {
	s.pair = pair
	s.conn = pair.DataConn
	s.session = pair.DataSession
	s.controlConn = pair.ControlConn
	if pair.HasIsolatedControl() {
		s.controlSess = pair.ControlSession
		return
	}
	s.controlSess = nil
}

func (s *Server) closeSession() {
	s.sessMu.Lock()
	pair := s.pair
	session := s.session
	controlSession := s.controlSess
	conn := s.conn
	controlConn := s.controlConn
	control := s.controlStrm
	controlStop := s.controlStop
	peers := s.peerSessions
	oldSessionID := s.sessionID
	s.peerSessions = make(map[string]*peerSession)
	s.pair, s.session, s.controlSess = nil, nil, nil
	s.conn, s.controlConn = nil, nil
	s.controlStrm, s.controlStop = nil, nil
	s.sessionID, s.deviceID = "", ""
	s.sessMu.Unlock()
	s.state.broadcast()
	if controlStop != nil {
		controlStop()
	}
	tunnelcore.NotifyControlClose(control)
	if pair != nil {
		_ = pair.Close()
	} else {
		closeServerPair(nil, session, controlSession)
		if conn != nil {
			_ = conn.Close()
		}
		if controlConn != nil {
			_ = controlConn.Close()
		}
	}
	if oldSessionID != "" {
		s.onClose(oldSessionID, "closed")
		s.trackPeerClose(oldSessionID, "closed")
	}
	for _, peer := range peers {
		s.closePeerSession(peer, "closed")
	}
}

func (s *Server) onData(data []byte) {
	s.sessMu.RLock()
	conn := s.conn
	s.sessMu.RUnlock()
	tunnelcore.PushData(conn, data)
}

func (s *Server) shutdown() {
	if s.done != nil {
		s.doneOnce.Do(func() { close(s.done) })
	}
	s.closeSession()
	if s.ln != nil {
		_ = s.ln.Close()
	}
}
