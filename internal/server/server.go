// Package server implements the olcrtc tunnel server logic.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"

	"github.com/openlibrecommunity/olcrtc/internal/control"
	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/handshake"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/openlibrecommunity/olcrtc/internal/tunnelcore"
)

const connectCommand = "connect"

// gracefulCloseTimeout bounds how long shutdown waits for the peer close
// notifications to reach the wire before it tears the transport down anyway.
// One peer costs a write plus a short flush wait, so this only has to cover a
// handful of them; a link too congested to drain in that time was not going to
// deliver the notification at all.
const gracefulCloseTimeout = 3 * time.Second

var (
	ErrKeyRequired         = runtime.ErrKeyRequired
	ErrKeySize             = runtime.ErrKeySize
	ErrSocks5AuthFailed    = errors.New("SOCKS5 auth failed")
	ErrSocks5ConnectFailed = errors.New("SOCKS5 connect failed")
	ErrInvalidTarget       = errors.New("invalid connect target")
)

// SessionOpenFunc is called after a successful handshake.
type SessionOpenFunc func(sessionID, deviceID string, claims map[string]any)

// SessionCloseFunc is called when a session is torn down.
type SessionCloseFunc func(sessionID, reason string)

// TrafficFunc is called once per tunnel stream after both copy loops finish.
type TrafficFunc func(sessionID, addr string, bytesIn, bytesOut uint64)

// HealthFunc is called when the server control health snapshot changes.
type HealthFunc func(control.Status)

// Server handles incoming tunnel connections and proxies their traffic.
type Server struct {
	baseCtx context.Context //nolint:containedctx // server-lifetime context for reconnect goroutines
	ln      transport.Transport
	peerLn  transport.PeerTransport
	keys    *crypto.KeySet
	pair    *tunnelcore.SessionPair
	conn    *muxconn.Conn

	controlConn *muxconn.Conn
	session     *smux.Session
	controlSess *smux.Session
	controlStrm *smux.Stream
	controlStop context.CancelFunc
	sessMu      sync.RWMutex

	peerSessions map[string]*peerSession
	// peerLimitWarn rate-limits the peer-cap warning.
	peerLimitWarn atomic.Int64
	peersMu       sync.Mutex
	peerStats     map[string]peerStat
	reinstallMu   sync.Mutex
	wg            sync.WaitGroup
	authHook      handshake.AuthFunc
	onOpen        SessionOpenFunc
	onClose       SessionCloseFunc
	onTraffic     TrafficFunc
	deviceID      string
	sessionID     string

	dnsServer      string
	resolver       *net.Resolver
	socksProxyAddr string
	socksProxyPort int
	socksProxyUser string
	socksProxyPass string
	liveness       control.Config
	health         *runtime.HealthTracker
	state          stateGate
	done           chan struct{}
	doneOnce       sync.Once
}

// Config holds runtime configuration for [Run].
type Config struct {
	Transport        string
	Provider         string
	RoomURL          string
	ChannelID        string
	KeyHex           string
	DNSServer        string
	Resolver         *net.Resolver
	SOCKSProxyAddr   string
	SOCKSProxyPort   int
	SOCKSProxyUser   string
	SOCKSProxyPass   string
	TransportOptions transport.Options
	Engine           string
	URL              string
	Token            string
	ProviderToken    string
	Liveness         control.Config
	Traffic          transport.TrafficConfig
	AuthHook         handshake.AuthFunc
	OnSessionOpen    SessionOpenFunc
	OnSessionClose   SessionCloseFunc
	OnTraffic        TrafficFunc
	OnHealth         HealthFunc
}

// Run starts the server with the given configuration.
func Run(ctx context.Context, cfg Config) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The link and the control loops run on their own context, cancelled only
	// once the goodbyes are out. control.Run closes its stream as soon as its
	// context is done, so hanging the control loops off runCtx meant the signal
	// that starts a graceful shutdown also slammed shut the very stream the
	// closing notification has to be written to - the write failed with
	// io.ErrClosedPipe every time and peers were left to discover the retired
	// room through missed liveness pongs instead of switching immediately.
	sessCtx, sessCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer sessCancel()
	keys, err := tunnelcore.SetupKeySet(cfg.KeyHex, crypto.Server)
	if err != nil {
		return fmt.Errorf("setup key set: %w", err)
	}
	hook := cfg.AuthHook
	if hook == nil {
		hook = defaultAuthHook
	}
	onOpen := cfg.OnSessionOpen
	if onOpen == nil {
		onOpen = func(string, string, map[string]any) {}
	}
	onClose := cfg.OnSessionClose
	if onClose == nil {
		onClose = func(string, string) {}
	}
	onTraffic := cfg.OnTraffic
	if onTraffic == nil {
		onTraffic = func(string, string, uint64, uint64) {}
	}
	s := &Server{
		keys: keys, authHook: hook, onOpen: onOpen, onClose: onClose, onTraffic: onTraffic,
		dnsServer: cfg.DNSServer, resolver: tunnelcore.Resolver(cfg.Resolver, cfg.DNSServer),
		socksProxyAddr: cfg.SOCKSProxyAddr, socksProxyPort: cfg.SOCKSProxyPort,
		socksProxyUser: cfg.SOCKSProxyUser, socksProxyPass: cfg.SOCKSProxyPass,
		liveness: cfg.Liveness, health: runtime.NewHealthTracker(cfg.OnHealth),
		peerSessions: make(map[string]*peerSession), peerStats: make(map[string]peerStat),
		done: make(chan struct{}),
	}
	defer func() {
		s.shutdown()
		s.wg.Wait()
	}()
	if err := s.bringUpLink(sessCtx, cfg, cancel); err != nil {
		return err
	}
	// closeSession tells every peer we are leaving, and that notification is a
	// frame written INTO the tunnel: NotifyControlClose writes it and waits for
	// the link to drain. The deferred shutdown above closes the transport, so
	// letting it run concurrently drops exactly the frame that lets a client
	// fail over to its warm standby immediately - it is left to discover the
	// retired room the slow way, through missed liveness pongs. Wait for the
	// notifications to reach the wire first, bounded so a wedged link cannot
	// hang shutdown.
	notified := make(chan struct{})
	go func() {
		defer close(notified)
		<-runCtx.Done()
		s.closeSession()
	}()
	s.serve(runCtx)
	// serve can also return without the context being cancelled (a dead link);
	// cancel so the notifier is always released.
	cancel()
	select {
	case <-notified:
	case <-time.After(gracefulCloseTimeout):
		logger.Warnf("server shutdown: peer close notifications still pending after %s", gracefulCloseTimeout)
	}
	// Goodbyes are out (or timed out): the control loops may go now.
	sessCancel()
	return nil
}
