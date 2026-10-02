package jitsi

import (
	"context"
	"fmt"
	"time"

	"github.com/zarazaex69/j"

	"github.com/openlibrecommunity/olcrtc/internal/engine"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
)

const reconnectJoinTimeout = 30 * time.Second

func (s *Session) requestReconnect(reason string) {
	if s.closed.Load() || s.reconnecting.Load() {
		return
	}
	request := s.Request(false, false)
	if request == engine.ReconnectRejected {
		s.signalEnded(reason)
		return
	}
	logger.Infof("jitsi reconnect requested: %s", reason)
	if request == engine.ReconnectQueued {
		s.bridgeReady.Store(false)
	}
}

// requestReconnectGen ignores reports from recv loops bound to an old bridge.
func (s *Session) requestReconnectGen(gen uint64, reason string) {
	if cur := s.bridgeGen.Load(); cur != gen {
		logger.Debugf("jitsi: ignoring stale reconnect request (gen=%d live=%d): %s", gen, cur, reason)
		return
	}
	s.requestReconnect(reason)
}

func (s *Session) reconnect(ctx context.Context) error {
	// Close cancels no context this path uses, so without an explicit check a
	// reconnect racing Close joins the MUC again and installs an XMPP session
	// that nothing will ever close.
	if s.closed.Load() {
		return ErrSessionClosed
	}
	if !s.reconnecting.CompareAndSwap(false, true) {
		return nil
	}
	defer s.reconnecting.Store(false)

	s.bridgeReady.Store(false)
	s.teardownPC()
	s.localEpoch.Store(randomEpoch())
	s.peerEpoch.Store(0)
	s.resetPeerEpochs()
	s.drainSendQueue()

	// A full JoinMUC is required because a lightweight rejoin skips Jicofo's
	// focus allocation after an idle conference has been terminated.
	if old := s.setJSession(nil); old != nil {
		_ = old.Close()
	}

	logger.Infof("jitsi: rejoin %s/%s (non-blocking) ...", s.host, s.room)
	joinCtx, joinCancel := context.WithTimeout(ctx, reconnectJoinTimeout)
	jSess, err := j.JoinMUC(joinCtx, j.Config{
		Host:       s.host,
		Room:       s.room,
		Nick:       s.name,
		Debug:      logger.IsVerbose(),
		HTTPClient: s.httpClient,
	})
	joinCancel()
	if err != nil {
		logger.Warnf("jitsi: rejoin failed: %v - full reconnect", err)
		return s.reconnectFull(ctx)
	}
	if !s.installSession(jSess) {
		return ErrSessionClosed
	}

	const reinitiateTimeout = 30 * time.Second
	reinitCtx, reinitCancel := context.WithTimeout(ctx, reinitiateTimeout)
	_, err = jSess.WaitJingleReinitiate(reinitCtx)
	reinitCancel()
	if err != nil {
		logger.Warnf("jitsi: wait reinitiate failed: %v - full reconnect", err)
		return s.reconnectFull(ctx)
	}

	if err := s.reinitiateBridge(ctx, jSess); err != nil {
		return err
	}
	s.finishReconnect(jSess, "reinitiate")
	return nil
}

func (s *Session) reinitiateBridge(ctx context.Context, jSess *j.Session) error {
	needBridge := s.onData != nil || s.onPeerData != nil
	wantVideo := s.shouldRequestVideo()
	sctpBridge := (needBridge || wantVideo) && jSess.ColibriWS == ""
	if s.shouldNegotiatePC(needBridge) || wantVideo {
		if err := s.negotiatePC(ctx, jSess, sctpBridge); err != nil {
			logger.Warnf("jitsi: negotiate after reinitiate failed: %v - full reconnect", err)
			return s.reconnectFull(ctx)
		}
	}
	if sctpBridge {
		if err := s.openBridgeSCTP(ctx, jSess); err != nil {
			logger.Warnf("jitsi: bridge after reinitiate failed: %v - full reconnect", err)
			return s.reconnectFull(ctx)
		}
	} else if needBridge || wantVideo {
		if err := s.openBridgeWS(ctx, jSess); err != nil {
			logger.Warnf("jitsi: bridge after reinitiate failed: %v - full reconnect", err)
			return s.reconnectFull(ctx)
		}
	}
	return nil
}

// reconnectFull parks a joined MUC and returns errNoPeer when Jicofo has no
// second participant yet. The shared Reconnector treats that as non-failure.
func (s *Session) reconnectFull(ctx context.Context) error {
	if old := s.setJSession(nil); old != nil {
		_ = old.Close()
	}
	s.localEpoch.Store(randomEpoch())
	s.peerEpoch.Store(0)
	s.resetPeerEpochs()
	s.drainSendQueue()

	const fullReconnectTimeout = 60 * time.Second
	logger.Infof("jitsi: full reconnect %s/%s as %s ...", s.host, s.room, s.name)

	joinCtx, joinCancel := context.WithTimeout(ctx, reconnectJoinTimeout)
	jSess, err := j.JoinMUC(joinCtx, j.Config{
		Host:       s.host,
		Room:       s.room,
		Nick:       s.name,
		Debug:      logger.IsVerbose(),
		HTTPClient: s.httpClient,
	})
	joinCancel()
	if err != nil {
		return fmt.Errorf("jitsi join: %w", err)
	}
	bctx, bcancel := context.WithTimeout(ctx, fullReconnectTimeout)
	_, err = jSess.Conn.WaitJingle(bctx)
	bcancel()
	if err != nil {
		if !s.installSession(jSess) {
			return ErrSessionClosed
		}
		s.goLaunch(s.waitForJingle)
		return errNoPeer
	}

	if err := s.completeJingleSetup(ctx, jSess); err != nil {
		_ = jSess.Close()
		return fmt.Errorf("jitsi setup after full reconnect: %w", err)
	}
	if !s.installSession(jSess) {
		return ErrSessionClosed
	}
	s.finishReconnect(jSess, "full")
	return nil
}

// installSession publishes jSess as the live XMPP session, or closes it when
// Close has already run. The re-check after the store is what makes it safe:
// a Close that lands in between saw the previous value and closed that one,
// so whichever order the two take, exactly one of them closes jSess. Without
// it a rejoin that spent a minute waiting for Jicofo could install a session
// after teardown, and goLaunch then refuses the goroutines that would have
// noticed.
func (s *Session) installSession(jSess *j.Session) bool {
	if s.closed.Load() {
		_ = jSess.Close()
		return false
	}
	if old := s.setJSession(jSess); old != nil && old != jSess {
		_ = old.Close()
	}
	if s.closed.Load() {
		if current := s.setJSession(nil); current != nil {
			_ = current.Close()
		}
		return false
	}
	return true
}

func (s *Session) finishReconnect(jSess *j.Session, mode string) {
	s.peerEndpoint.Store(nil)
	s.peerVideoSSRC.Store(0)
	s.markBridgeReady()
	s.goLaunch(s.recvLoop)
	if err := s.Send(nil); err != nil {
		logger.Debugf("jitsi: epoch announce failed: %v", err)
	}
	s.notifyReconnect()
	s.lastReconnectAt.Store(time.Now().UnixNano())
	logger.Infof("jitsi: reconnected %s/%s (%s); colibri-ws=%s", s.host, s.room, mode, jSess.ColibriWS)
}

// drainSendQueue empties every queue in place. In place matters: a sender
// blocked on a full queue holds that very channel, and only room in it (or
// the session ending) lets the sender go.
func (s *Session) drainSendQueue() {
	drain(s.sendQueue)
	s.peerQueueMu.Lock()
	queues := make([]*peerQueue, 0, len(s.peerQueues))
	for _, pq := range s.peerQueues {
		queues = append(queues, pq)
	}
	s.peerQueueMu.Unlock()
	for _, pq := range queues {
		drain(pq.ch)
	}
}

func drain(ch <-chan []byte) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
