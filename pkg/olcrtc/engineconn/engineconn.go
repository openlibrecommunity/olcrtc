// Package engineconn exposes a raw unencrypted connection over an olcrtc engine.
//
// This package does not provide tunnel encryption, handshake, smux, SOCKS, or
// liveness. Use package client or tunnel for the complete product stack.
// New registers the built-in providers and engines automatically.
// RegisterDefaults is only needed after custom registry manipulation or extension.
package engineconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/openlibrecommunity/olcrtc/internal/auth"
	"github.com/openlibrecommunity/olcrtc/internal/engine"
	enginebuiltin "github.com/openlibrecommunity/olcrtc/internal/engine/builtin"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

var (
	// ErrURLRequired is returned when direct mode is used without a URL.
	ErrURLRequired = errors.New("engineconn: URL required when using direct engine mode")
	// ErrTokenRequired is returned when direct mode is used without a token.
	ErrTokenRequired = errors.New("engineconn: Token required when using direct engine mode")
	// ErrSessionEnded is returned from Read when the engine session ends.
	ErrSessionEnded = errors.New("engineconn: session ended")
)

// Config is the input to [New]. Provider mode resolves engine credentials from
// RoomURL. Direct mode uses Engine, URL, and Token when Provider is empty.
type Config struct {
	Provider      string
	RoomURL       string
	ProviderToken string
	Engine        string
	URL           string
	Token         string
	Name          string
	DNSServer     string
	Resolver      protect.Lookup
	ProxyAddr     string
	ProxyPort     int
}

// Session owns one raw engine session.
//
// Inbound engine data is delivered through a synchronous pipe that only the
// stream returned by [Session.Dial] drains. A consumer that uses Connect and
// Send without ever calling Dial therefore blocks the engine's receive
// callback on the first inbound packet; Close releases it. Call Dial for any
// session that receives data.
type Session struct {
	inner engine.Session
	pr    *io.PipeReader
	pw    *io.PipeWriter

	watchOnce sync.Once
	endedMu   sync.RWMutex
	onEnded   func(string)
}

// RegisterDefaults registers all built-in providers and engines.
// New calls it automatically. Manual calls are only needed after custom registry
// manipulation or extension. It is safe to call multiple times.
func RegisterDefaults() {
	enginebuiltin.RegisterDefaults()
}

// New creates a disconnected raw engine session.
func New(ctx context.Context, cfg Config) (*Session, error) {
	RegisterDefaults()
	cfg.Resolver = resolverFor(cfg)
	if cfg.Provider != "" {
		return newWithProvider(ctx, cfg)
	}
	return newDirect(ctx, cfg)
}

func resolverFor(cfg Config) protect.Lookup {
	if cfg.Resolver != nil {
		return cfg.Resolver
	}
	return protect.NewResolver(cfg.DNSServer)
}

func newWithProvider(ctx context.Context, cfg Config) (*Session, error) {
	provider, err := auth.Get(cfg.Provider)
	if err != nil {
		return nil, fmt.Errorf("engineconn: provider %q not registered: %w", cfg.Provider, err)
	}
	providerCfg := auth.Config{
		RoomURL: cfg.RoomURL, Name: cfg.Name, Token: cfg.ProviderToken,
		DNSServer: cfg.DNSServer, Resolver: cfg.Resolver,
		ProxyAddr: cfg.ProxyAddr, ProxyPort: cfg.ProxyPort,
	}
	creds, err := provider.Issue(ctx, providerCfg)
	if err != nil {
		return nil, fmt.Errorf("engineconn: provider issue: %w", err)
	}
	return newSession(ctx, cfg, provider.Engine(), creds, func(refreshCtx context.Context) (engine.Credentials, error) {
		fresh, refreshErr := provider.Issue(refreshCtx, providerCfg)
		if refreshErr != nil {
			return engine.Credentials{}, fmt.Errorf("engineconn: provider refresh: %w", refreshErr)
		}
		return engine.Credentials{URL: fresh.URL, Token: fresh.Token, Extra: fresh.Extra}, nil
	})
}

func newDirect(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.URL == "" {
		return nil, ErrURLRequired
	}
	if cfg.Token == "" {
		return nil, ErrTokenRequired
	}
	engineName := cfg.Engine
	if engineName == "" {
		engineName = "livekit"
	}
	return newSession(ctx, cfg, engineName, auth.Credentials{URL: cfg.URL, Token: cfg.Token}, nil)
}

func newSession(
	ctx context.Context,
	cfg Config,
	engineName string,
	creds auth.Credentials,
	refresh func(context.Context) (engine.Credentials, error),
) (*Session, error) {
	pr, pw := io.Pipe()
	inner, err := engine.New(ctx, engineName, engine.Config{
		URL: creds.URL, Token: creds.Token, Extra: creds.Extra, Name: cfg.Name,
		OnData: func(data []byte) { _, _ = pw.Write(data) }, DNSServer: cfg.DNSServer,
		Resolver: cfg.Resolver, ProxyAddr: cfg.ProxyAddr, ProxyPort: cfg.ProxyPort,
		Refresh: refresh,
	})
	if err != nil {
		_ = pw.CloseWithError(err)
		return nil, fmt.Errorf("engineconn: engine %q: %w", engineName, err)
	}
	return &Session{inner: inner, pr: pr, pw: pw}, nil
}

// Dial connects and returns a raw engine byte stream. The stream intentionally
// implements io.ReadWriteCloser rather than net.Conn because engine writes do
// not support context cancellation or interruptible deadlines.
//
// Calling Dial more than once returns another handle to the same stream; the
// reconnect watcher is started only by the first call, so repeated dials do
// not stack watchers on one session.
func (s *Session) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	s.inner.SetEndedCallback(s.handleEnded)
	if err := s.Connect(ctx); err != nil {
		return nil, err
	}
	s.watchOnce.Do(func() { go s.inner.WatchConnection(ctx) })
	return &stream{s: s}, nil
}

// Connect establishes the engine connection.
func (s *Session) Connect(ctx context.Context) error {
	if err := s.inner.Connect(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}

// Send sends data through the raw engine.
func (s *Session) Send(data []byte) error {
	if err := s.inner.Send(data); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	return nil
}

// Close closes the engine session.
func (s *Session) Close() error {
	_ = s.pw.CloseWithError(net.ErrClosed)
	if err := s.inner.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

// WatchConnection monitors engine reconnects until ctx is canceled.
func (s *Session) WatchConnection(ctx context.Context) {
	s.inner.WatchConnection(ctx)
}

// CanSend reports whether the engine can accept data.
func (s *Session) CanSend() bool {
	return s.inner.CanSend()
}

// SetEndedCallback registers a callback for permanent engine termination.
func (s *Session) SetEndedCallback(callback func(reason string)) {
	s.endedMu.Lock()
	s.onEnded = callback
	s.endedMu.Unlock()
	s.inner.SetEndedCallback(s.handleEnded)
}

func (s *Session) handleEnded(reason string) {
	_ = s.pw.CloseWithError(ErrSessionEnded)
	s.endedMu.RLock()
	callback := s.onEnded
	s.endedMu.RUnlock()
	if callback != nil {
		callback(reason)
	}
}

// SetShouldReconnect controls whether automatic reconnection is attempted.
func (s *Session) SetShouldReconnect(check func() bool) {
	s.inner.SetShouldReconnect(check)
}

type stream struct {
	s *Session
}

func (c *stream) Read(data []byte) (int, error) {
	n, err := c.s.pr.Read(data)
	if err != nil {
		return n, fmt.Errorf("read: %w", err)
	}
	return n, nil
}

func (c *stream) Write(data []byte) (int, error) {
	if err := c.s.inner.Send(data); err != nil {
		return 0, fmt.Errorf("write: %w", err)
	}
	return len(data), nil
}

func (c *stream) Close() error {
	return c.s.Close()
}

var _ io.ReadWriteCloser = (*stream)(nil)
