// Package olcrtc exposes olcrtc as an embeddable Go library.
//
// Typical usage - obtain a [net.Conn]-compatible handle and dial:
//
//	sess, err := olcrtc.New(ctx, olcrtc.Config{
//	    Engine: "livekit",
//	    URL:    "wss://sfu.example/",
//	    Token:  "<livekit-jwt>",
//	})
//	if err != nil { ... }
//	conn, err := sess.Dial(ctx)  // blocks until WebRTC data channel is ready
//	// conn implements net.Conn - pass it to sing-box / any io.ReadWriter consumer
//
// Built-in auth providers (jitsi, telemost, wbstream):
//
//	sess, err := olcrtc.New(ctx, olcrtc.Config{
//	    Auth:   "jitsi",
//	    // Use meet.small-dm.ru, meet1.arbitr.ru, or meet.handyweb.org - whichever works in your network
//	    RoomID: "https://meet.small-dm.ru/myroom",
//	})
//
// Import the implementations you need via blank imports, or call [RegisterDefaults]:
//
//	import (
//	    _ "github.com/openlibrecommunity/olcrtc/internal/engine/jitsi"
//	    _ "github.com/openlibrecommunity/olcrtc/internal/auth/jitsi"
//	)
package olcrtc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/openlibrecommunity/olcrtc/internal/auth"
	"github.com/openlibrecommunity/olcrtc/internal/engine"
	enginebuiltin "github.com/openlibrecommunity/olcrtc/internal/engine/builtin"
)

var (
	// ErrURLRequired is returned when direct mode is used without a URL.
	ErrURLRequired = errors.New("olcrtc: URL required when using direct engine mode")
	// ErrTokenRequired is returned when direct mode is used without a token.
	ErrTokenRequired = errors.New("olcrtc: Token required when using direct engine mode")
	// ErrRoomCreationUnsupported is returned when the auth provider cannot create rooms.
	ErrRoomCreationUnsupported = errors.New("olcrtc: auth provider does not support room creation")
	// ErrSessionEnded is returned from Read/Write when the session has ended permanently.
	ErrSessionEnded = errors.New("olcrtc: session ended")
)

// Config is the input to [New].
type Config struct {
	// --- built-in auth mode ---
	// Auth is the name of a registered auth provider ("jitsi", "telemost", "wbstream").
	// When set, RoomID is forwarded to the provider as the room reference.
	Auth   string
	RoomID string

	// --- direct engine mode (Auth == "") ---
	// Engine selects the SFU protocol ("livekit", "goolom", "jitsi").
	// Defaults to "livekit" when Auth is empty.
	Engine string
	URL    string
	Token  string

	// --- common ---
	// Name is the display name used when joining the room.
	Name string
	// DNSServer is an optional custom DNS resolver (e.g. "8.8.8.8:53").
	DNSServer string
	// ProxyAddr / ProxyPort configure an outbound SOCKS5 proxy.
	ProxyAddr string
	ProxyPort int
}

// Session is the library handle returned by [New].
// Call [Session.Dial] to connect and obtain a [net.Conn].
type Session struct {
	inner        engine.Session
	pr           *io.PipeReader
	pw           *io.PipeWriter
	authProvider auth.Provider
	authCfg      auth.Config
}

// RegisterDefaults registers all built-in engines and auth providers.
// Call once at program start if you want the full set without manual blank
// imports. Safe to call multiple times.
func RegisterDefaults() {
	enginebuiltin.RegisterDefaults()
}

// New creates a Session from cfg. The session is not connected yet; call
// [Session.Connect] when ready.
func New(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.Auth != "" {
		return newWithAuth(ctx, cfg)
	}
	return newDirect(ctx, cfg)
}

func newWithAuth(ctx context.Context, cfg Config) (*Session, error) {
	p, err := auth.Get(cfg.Auth)
	if err != nil {
		return nil, fmt.Errorf("olcrtc: auth provider %q not registered: %w", cfg.Auth, err)
	}

	authCfg := auth.Config{
		RoomURL:   cfg.RoomID,
		Name:      cfg.Name,
		Token:     cfg.Token,
		DNSServer: cfg.DNSServer,
		ProxyAddr: cfg.ProxyAddr,
		ProxyPort: cfg.ProxyPort,
	}

	creds, err := p.Issue(ctx, authCfg)
	if err != nil {
		return nil, fmt.Errorf("olcrtc: auth issue: %w", err)
	}

	pr, pw := io.Pipe()
	engineName := p.Engine()
	sess, err := engine.New(ctx, engineName, engine.Config{
		URL:       creds.URL,
		Token:     creds.Token,
		Name:      cfg.Name,
		Extra:     creds.Extra,
		OnData:    func(data []byte) { _, _ = pw.Write(data) },
		DNSServer: cfg.DNSServer,
		ProxyAddr: cfg.ProxyAddr,
		ProxyPort: cfg.ProxyPort,
		Refresh: func(rCtx context.Context) (engine.Credentials, error) {
			fresh, freshErr := p.Issue(rCtx, authCfg)
			if freshErr != nil {
				return engine.Credentials{}, fmt.Errorf("olcrtc: auth refresh: %w", freshErr)
			}
			return engine.Credentials{URL: fresh.URL, Token: fresh.Token, Extra: fresh.Extra}, nil
		},
	})
	if err != nil {
		_ = pw.CloseWithError(err)
		return nil, fmt.Errorf("olcrtc: engine %q: %w", engineName, err)
	}

	return &Session{inner: sess, pr: pr, pw: pw, authProvider: p, authCfg: authCfg}, nil
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

	pr, pw := io.Pipe()
	sess, err := engine.New(ctx, engineName, engine.Config{
		URL:       cfg.URL,
		Token:     cfg.Token,
		Name:      cfg.Name,
		OnData:    func(data []byte) { _, _ = pw.Write(data) },
		DNSServer: cfg.DNSServer,
		ProxyAddr: cfg.ProxyAddr,
		ProxyPort: cfg.ProxyPort,
	})
	if err != nil {
		_ = pw.CloseWithError(err)
		return nil, fmt.Errorf("olcrtc: engine %q: %w", engineName, err)
	}

	return &Session{inner: sess, pr: pr, pw: pw}, nil
}

// Dial connects and returns a [net.Conn] backed by the WebRTC data channel.
// It combines [Session.Connect] + wrapping in a single call.
// The connection watcher runs in the background for the lifetime of ctx;
// when the session ends permanently, Read will return an error.
func (s *Session) Dial(ctx context.Context) (net.Conn, error) {
	s.inner.SetEndedCallback(func(_ string) {
		_ = s.pw.CloseWithError(ErrSessionEnded)
	})
	if err := s.Connect(ctx); err != nil {
		return nil, err
	}
	go s.inner.WatchConnection(ctx)
	return &conn{s: s}, nil
}

// Connect establishes the WebRTC connection. Blocks until the data channel (or
// media) is ready, or ctx is cancelled.
func (s *Session) Connect(ctx context.Context) error {
	if err := s.inner.Connect(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}

// Send queues data for transmission over the data channel.
func (s *Session) Send(data []byte) error {
	if err := s.inner.Send(data); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	return nil
}

// Close tears down the session and releases all resources.
func (s *Session) Close() error {
	if err := s.inner.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

// WatchConnection monitors the connection and handles reconnects. Run in a
// goroutine alongside Connect.
func (s *Session) WatchConnection(ctx context.Context) {
	s.inner.WatchConnection(ctx)
}

// CanSend reports whether the session is ready to accept outgoing data.
func (s *Session) CanSend() bool {
	return s.inner.CanSend()
}

// SetEndedCallback registers a function called when the session ends
// permanently (after reconnect exhaustion or explicit close).
func (s *Session) SetEndedCallback(cb func(reason string)) {
	s.inner.SetEndedCallback(cb)
}

// SetShouldReconnect controls whether automatic reconnection is attempted.
func (s *Session) SetShouldReconnect(fn func() bool) {
	s.inner.SetShouldReconnect(fn)
}

// CreateRoom creates a new room via the auth provider and returns the room ID.
// Only works when Auth names a provider that supports room creation. Built-in
// providers currently return [ErrRoomCreationUnsupported].
func CreateRoom(ctx context.Context, authName string) (string, error) {
	p, err := auth.Get(authName)
	if err != nil {
		return "", fmt.Errorf("olcrtc: auth provider %q not registered: %w", authName, err)
	}
	creator, ok := p.(auth.RoomCreator)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrRoomCreationUnsupported, authName)
	}
	roomID, err := creator.CreateRoom(ctx, auth.Config{})
	if err != nil {
		return "", fmt.Errorf("olcrtc: create room: %w", err)
	}
	return roomID, nil
}
