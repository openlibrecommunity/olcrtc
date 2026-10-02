// Package tunnel exposes the olcrtc server tunnel as an embeddable Go library.
// New registers the built-in providers, engines, and transports automatically.
// RegisterDefaults is only needed after custom registry manipulation or extension.
package tunnel

import (
	"context"
	"fmt"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/app/session"
	"github.com/openlibrecommunity/olcrtc/internal/control"
	"github.com/openlibrecommunity/olcrtc/internal/handshake"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
	"github.com/openlibrecommunity/olcrtc/internal/server"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/openlibrecommunity/olcrtc/internal/transport/seichannel"
	"github.com/openlibrecommunity/olcrtc/internal/transport/videochannel"
	"github.com/openlibrecommunity/olcrtc/internal/transport/vp8channel"
)

// TransportOptions is implemented by the built-in public option structs.
type TransportOptions interface {
	transportOptions()
}

// VideoOptions configures videochannel.
type VideoOptions struct {
	Width      int
	Height     int
	FPS        int
	QRSize     int
	QRRecovery string
	Codec      string
	TileModule int
	TileRS     int
}

func (VideoOptions) transportOptions() {}

// VP8Options configures vp8channel.
type VP8Options struct {
	FPS       int
	BatchSize int
}

func (VP8Options) transportOptions() {}

// SEIOptions configures seichannel.
type SEIOptions struct {
	FPS          int
	BatchSize    int
	FragmentSize int
	AckTimeoutMS int
}

func (SEIOptions) transportOptions() {}

// AuthFunc authorizes a client after CLIENT_HELLO.
type AuthFunc func(deviceID string, claims map[string]any) (sessionID string, err error)

// SessionOpenFunc is called after a successful handshake.
type SessionOpenFunc func(sessionID, deviceID string, claims map[string]any)

// SessionCloseFunc is called when a session ends.
type SessionCloseFunc func(sessionID, reason string)

// TrafficFunc is called after both copy loops for a tunnel stream finish.
type TrafficFunc func(sessionID, addr string, bytesIn, bytesOut uint64)

// HealthStatus is a control-stream health snapshot.
type HealthStatus = control.Status

// HealthFunc is called when the control-stream health snapshot changes.
type HealthFunc func(HealthStatus)

// LivenessConfig controls control-stream ping and pong checks.
type LivenessConfig struct {
	Interval time.Duration
	Timeout  time.Duration
	Failures int
}

// TrafficConfig controls optional payload limits and send pacing.
type TrafficConfig struct {
	MaxPayloadSize int
	MinDelay       time.Duration
	MaxDelay       time.Duration
}

// Config holds all server tunnel capabilities.
type Config struct {
	Transport        string
	Provider         string
	RoomURL          string
	ChannelID        string
	Engine           string
	URL              string
	Token            string
	ProviderToken    string
	KeyHex           string
	DNSServer        string
	Resolver         protect.Lookup
	SOCKSProxyAddr   string
	SOCKSProxyPort   int
	SOCKSProxyUser   string
	SOCKSProxyPass   string
	TransportOptions TransportOptions
	Liveness         LivenessConfig
	Traffic          TrafficConfig
	AuthHook         AuthFunc
	OnSessionOpen    SessionOpenFunc
	OnSessionClose   SessionCloseFunc
	OnTraffic        TrafficFunc
	OnHealth         HealthFunc
}

type runner func(context.Context, server.Config) error

// Server is an embeddable tunnel server.
type Server struct {
	cfg Config
	run runner
}

// New returns a server configured by cfg.
func New(cfg Config) *Server {
	RegisterDefaults()
	return &Server{cfg: cfg, run: server.Run}
}

// Run starts the server and blocks until ctx is canceled or the provider ends.
func (s *Server) Run(ctx context.Context) error {
	if err := s.run(ctx, toServerConfig(s.cfg)); err != nil {
		return fmt.Errorf("tunnel: %w", err)
	}
	return nil
}

func toServerConfig(cfg Config) server.Config {
	return server.Config{
		Transport: cfg.Transport, Provider: cfg.Provider, RoomURL: cfg.RoomURL,
		ChannelID: cfg.ChannelID, Engine: cfg.Engine, URL: cfg.URL, Token: cfg.Token,
		ProviderToken: cfg.ProviderToken, KeyHex: cfg.KeyHex, DNSServer: cfg.DNSServer,
		Resolver: cfg.Resolver, SOCKSProxyAddr: cfg.SOCKSProxyAddr,
		SOCKSProxyPort: cfg.SOCKSProxyPort, SOCKSProxyUser: cfg.SOCKSProxyUser,
		SOCKSProxyPass: cfg.SOCKSProxyPass, TransportOptions: toTransportOptions(cfg.TransportOptions),
		Liveness: control.Config{
			Interval: cfg.Liveness.Interval, Timeout: cfg.Liveness.Timeout, Failures: cfg.Liveness.Failures,
		},
		Traffic: transport.TrafficConfig{
			MaxPayloadSize: cfg.Traffic.MaxPayloadSize,
			MinDelay:       cfg.Traffic.MinDelay, MaxDelay: cfg.Traffic.MaxDelay,
		},
		AuthHook:       handshake.AuthFunc(cfg.AuthHook),
		OnSessionOpen:  server.SessionOpenFunc(cfg.OnSessionOpen),
		OnSessionClose: server.SessionCloseFunc(cfg.OnSessionClose),
		OnTraffic:      server.TrafficFunc(cfg.OnTraffic), OnHealth: server.HealthFunc(cfg.OnHealth),
	}
}

func toTransportOptions(options TransportOptions) transport.Options {
	switch value := options.(type) {
	case VideoOptions:
		return videochannel.Options(value)
	case VP8Options:
		return vp8channel.Options(value)
	case SEIOptions:
		return seichannel.Options(value)
	default:
		return nil
	}
}

// RegisterDefaults registers the built-in providers, engines, and transports.
// New calls it automatically. Manual calls are only needed after custom registry
// manipulation or extension. It is safe to call multiple times.
func RegisterDefaults() {
	session.RegisterDefaults()
}
