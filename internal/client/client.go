// Package client implements the local SOCKS5 client side of the olcrtc tunnel.
package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/xtaci/smux"

	"github.com/openlibrecommunity/olcrtc/internal/control"
	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/openlibrecommunity/olcrtc/internal/tunnelcore"
)

var (
	ErrConnectFailed           = errors.New("tunnel connection failed")
	ErrProxyAuth               = errors.New("SOCKS proxy auth failed")
	ErrKeySize                 = runtime.ErrKeySize
	ErrInvalidSOCKSVersion     = errors.New("invalid socks version")
	ErrUnsupportedSOCKSCommand = errors.New("unsupported socks command")
	ErrUnsupportedAddressType  = errors.New("unsupported address type")
	ErrRemoteNotReady          = errors.New("remote not ready")
	ErrSOCKSAuthFailed         = errors.New("SOCKS5 authentication failed")
	ErrSOCKSCredTooLong        = errors.New("socks5 user/pass exceeds 255 bytes")
	ErrEmptySOCKSDomain        = errors.New("empty socks5 domain")
)

const (
	reconnectProvider = "provider"
	reconnectLiveness = "liveness"
	reconnectFallback = "liveness-fallback"
)

const (
	defaultLivenessFallback = 30 * time.Second
	defaultShutdownGrace    = 5 * time.Second
)

// Client handles local SOCKS5 connections and tunnels them to the server.
type Client struct {
	ln          transport.Transport
	keys        *crypto.KeySet
	pair        *tunnelcore.SessionPair
	conn        *muxconn.Conn
	controlConn *muxconn.Conn
	session     *smux.Session
	controlSess *smux.Session
	controlStrm *smux.Stream
	controlStop context.CancelFunc
	sessMu      sync.RWMutex
	reconnectMu sync.Mutex
	health      *runtime.HealthTracker

	// controlLastPong is independent corroboration for the transport's fast
	// peer-restart heuristic, not a second session reconnect detector.
	controlLastPong  atomic.Value // time.Time
	deviceID         string
	sessionID        string
	claims           map[string]any
	dnsServer        string
	socksUser        string
	socksPass        string
	sessionReady     chan struct{}
	wg               sync.WaitGroup
	socksMu          sync.Mutex
	socksConns       map[net.Conn]struct{}
	socksClosed      bool
	livenessFallback time.Duration
	shutdownGrace    time.Duration
	fallbackPending  atomic.Bool

	// peerNoIPv6 latches once the exit answers "host unreachable" for an IPv6
	// literal. On a dual-stack machine the OS fires IPv6 first for nearly every
	// connection, so against an IPv4-only exit that is the bulk of all traffic:
	// each attempt otherwise costs a stream open plus a full round trip over a
	// tunnel whose capacity is orders of magnitude below a local socket, and it
	// can never succeed. Latched, the same connections are refused locally and
	// Happy Eyeballs falls back to IPv4 immediately. Cleared per session because
	// a different exit may have IPv6.
	peerNoIPv6 atomic.Bool
}

// HealthFunc is called when the client control health snapshot changes.
type HealthFunc func(control.Status)

// Config holds runtime configuration for [Run], [RunWithReady], and [RunWithAddress].
type Config struct {
	Transport        string
	Provider         string
	RoomURL          string
	ChannelID        string
	KeyHex           string
	LocalAddr        string
	DNSServer        string
	Resolver         *net.Resolver
	SOCKSUser        string
	SOCKSPass        string
	TransportOptions transport.Options
	Engine           string
	URL              string
	Token            string
	ProviderToken    string
	Liveness         control.Config
	Traffic          transport.TrafficConfig
	DeviceID         string
	DeviceIDPath     string
	Claims           map[string]any
	OnHealth         HealthFunc
}

// Run starts the client with the given configuration.
func Run(ctx context.Context, cfg Config) error {
	return RunWithAddress(ctx, cfg, nil)
}

// RunWithReady starts the client and invokes onReady after the SOCKS listener opens.
func RunWithReady(ctx context.Context, cfg Config, onReady func()) error {
	if onReady == nil {
		return RunWithAddress(ctx, cfg, nil)
	}
	return RunWithAddress(ctx, cfg, func(string) { onReady() })
}

// RunWithAddress starts the client and reports the actual SOCKS listener address.
func RunWithAddress(ctx context.Context, cfg Config, onReady func(actualAddr string)) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	keys, err := tunnelcore.SetupKeySet(cfg.KeyHex, crypto.Client)
	if err != nil {
		return fmt.Errorf("setup key set: %w", err)
	}
	deviceID, err := resolveDeviceID(cfg.DeviceID, cfg.DeviceIDPath)
	if err != nil {
		return fmt.Errorf("resolve device id: %w", err)
	}
	client := &Client{
		keys: keys, deviceID: deviceID, claims: cfg.Claims, dnsServer: cfg.DNSServer,
		socksUser: cfg.SOCKSUser, socksPass: cfg.SOCKSPass,
		health: runtime.NewHealthTracker(cfg.OnHealth), sessionReady: make(chan struct{}),
	}
	defer func() {
		cancel()
		client.shutdown()
	}()
	if bringUpErr := client.bringUpLink(runCtx, cfg, cancel); bringUpErr != nil {
		return bringUpErr
	}
	listener, err := (&net.ListenConfig{}).Listen(runCtx, "tcp4", cfg.LocalAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", cfg.LocalAddr, err)
	}
	defer func() { _ = listener.Close() }()
	actualAddr := listener.Addr().String()
	logger.Infof("SOCKS5 server listening on %s", actualAddr)
	if onReady != nil {
		onReady(actualAddr)
	}
	client.goTracked(func() { client.acceptLoop(runCtx, listener) })
	<-runCtx.Done()
	return nil
}

// registerSocksConn tracks conn so shutdown can close it, and enforces the
// concurrency cap. It reports false when the cap is reached or the client is
// tearing down; the caller then closes conn itself.
func (c *Client) registerSocksConn(conn net.Conn) bool {
	c.socksMu.Lock()
	defer c.socksMu.Unlock()
	if c.socksClosed {
		return false
	}
	if len(c.socksConns) >= maxSocksConns {
		logger.Warnf("SOCKS5: %d concurrent connections reached, refusing new ones", maxSocksConns)
		return false
	}
	if c.socksConns == nil {
		c.socksConns = make(map[net.Conn]struct{})
	}
	c.socksConns[conn] = struct{}{}
	return true
}

func (c *Client) unregisterSocksConn(conn net.Conn) {
	c.socksMu.Lock()
	delete(c.socksConns, conn)
	c.socksMu.Unlock()
}

// closeSocksConns closes every live SOCKS connection. Without it shutdown
// would have to wait out the negotiation deadline of a client that connected
// and then went quiet.
func (c *Client) closeSocksConns() {
	c.socksMu.Lock()
	conns := c.socksConns
	c.socksConns = nil
	c.socksClosed = true
	c.socksMu.Unlock()
	for conn := range conns {
		_ = conn.Close()
	}
}

func (c *Client) goTracked(fn func()) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		fn()
	}()
}

func resolveDeviceID(deviceID, path string) (string, error) {
	if deviceID != "" {
		return deviceID, nil
	}
	if path == "" {
		return uuid.NewString(), nil
	}
	data, err := os.ReadFile(path) // #nosec G304 - path is explicit user configuration
	if err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read device id %s: %w", path, err)
	}
	id := uuid.NewString()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("mkdir device id dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write device id %s: %w", path, err)
	}
	return id, nil
}
