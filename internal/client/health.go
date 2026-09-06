package client

import (
	"context"
	"errors"
	"time"

	"github.com/xtaci/smux"

	"github.com/openlibrecommunity/olcrtc/internal/control"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/openlibrecommunity/olcrtc/internal/tunnelcore"
)

func (c *Client) startControlLoop(
	ctx context.Context,
	cfg Config,
	cancel context.CancelFunc,
	stream *smux.Stream,
) {
	controlCtx, stop := context.WithCancel(ctx)
	c.sessMu.Lock()
	c.controlStop = stop
	c.sessMu.Unlock()
	pingInterval := cfg.Liveness.Interval
	if pingInterval <= 0 {
		pingInterval = control.DefaultInterval
	}
	runner := tunnelcore.ControlRunner{
		Transport: c.ln, Config: cfg.Liveness, Health: c.health,
		LogFields: func() string {
			c.sessMu.RLock()
			defer c.sessMu.RUnlock()
			return "role=client session=" + c.sessionID
		},
		OnPong: func(control.Health) {
			c.controlLastPong.Store(time.Now())
			c.notifyLinkHealth(false)
		},
		OnDeath: func(err error) {
			if errors.Is(err, control.ErrClosedByPeer) {
				// The peer (srv) left ON PURPOSE: the server gracefully retired this
				// room (a rotation). Don't hammer the now-dead room for the full
				// liveness-fallback window - end the session immediately (same as a
				// conference-end) so the supervisor fails over to the next room right
				// away. A genuine drop (no graceful close) still goes the hammer path.
				logger.Infof("control closed by peer (server retired this room) - failing over to next room now")
				cancel()
				return
			}
			c.handleReconnect(ctx, cfg, cancel, reconnectLiveness)
		},
	}
	c.goTracked(func() { c.watchControlStaleness(controlCtx, pingInterval) })
	c.goTracked(func() { runner.Run(controlCtx, stream) })
}

// watchControlStaleness is not a second reconnect detector. It supplies early
// session-specific evidence to LinkHealthObserver after two missed intervals,
// while control.Run retains sole ownership of session teardown and reconnect.
func (c *Client) watchControlStaleness(ctx context.Context, interval time.Duration) {
	const staleFactor = 2
	threshold := staleFactor * interval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last, ok := c.controlLastPong.Load().(time.Time)
			c.notifyLinkHealth(ok && time.Since(last) > threshold)
		}
	}
}

// Status returns the latest client-side control health snapshot.
func (c *Client) Status() control.Status {
	return c.health.Status()
}

func (c *Client) notifyLinkHealth(unhealthy bool) {
	if observer, ok := c.ln.(transport.LinkHealthObserver); ok {
		observer.NotifyLinkHealth(unhealthy)
	}
}

func (c *Client) shutdown() {
	c.sessMu.Lock()
	pair := c.pair
	controlStream := c.controlStrm
	controlStop := c.controlStop
	session := c.session
	controlSession := c.controlSess
	conn := c.conn
	controlConn := c.controlConn
	c.pair = nil
	c.controlStrm, c.controlStop = nil, nil
	c.session, c.controlSess = nil, nil
	c.conn, c.controlConn = nil, nil
	c.sessMu.Unlock()
	tunnelcore.NotifyControlClose(controlStream)
	if controlStop != nil {
		controlStop()
	}
	closeClientPair(pair, session, controlSession)
	if pair == nil {
		if conn != nil {
			_ = conn.Close()
		}
		if controlConn != nil {
			_ = controlConn.Close()
		}
	}
	if c.ln != nil {
		_ = c.ln.Close()
	}
	if controlStream != nil {
		_ = controlStream.Close()
	}
	c.closeSocksConns()
	c.waitGoroutines()
}

func (c *Client) waitGoroutines() {
	grace := c.shutdownGrace
	if grace <= 0 {
		grace = defaultShutdownGrace
	}
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		logger.Warnf("client shutdown: goroutines still running after %s", grace)
	}
}

func (c *Client) onData(data []byte) {
	c.sessMu.RLock()
	conn := c.conn
	c.sessMu.RUnlock()
	tunnelcore.PushData(conn, data)
}
