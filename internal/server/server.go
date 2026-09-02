// Package server implements the olcrtc tunnel server logic.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/xtaci/smux"

	"github.com/openlibrecommunity/olcrtc/internal/control"
	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/handshake"
	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/openlibrecommunity/olcrtc/internal/tunnelcore"
)

const connectCommand = "connect"

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
	keys    interface {
		SealInto([]byte, []byte, []byte) ([]byte, error)
		OpenInto([]byte, []byte, []byte) ([]byte, error)
	}
	pair *tunnelcore.SessionPair
	conn *muxconn.Conn

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
	KeyHex           string   // deprecated, use KeysHex
	KeysHex          []string // list of encryption keys
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

	// Build list of keys: prefer KeysHex if provided, fall back to KeyHex for backward compatibility
	keyHexes := cfg.KeysHex
	if len(keyHexes) == 0 && cfg.KeyHex != "" {
		keyHexes = []string{cfg.KeyHex}
	}

	keys, err := tunnelcore.SetupMultiKeySet(keyHexes, crypto.Server)
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
	if err := s.bringUpLink(runCtx, cfg, cancel); err != nil {
		return err
	}
	go func() {
		<-runCtx.Done()
		s.closeSession()
	}()
	s.serve(runCtx)
	return nil
}
