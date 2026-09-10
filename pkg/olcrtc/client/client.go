// Package client exposes the olcrtc SOCKS5 tunnel client as an embeddable Go library.
// New registers the built-in providers, engines, and transports automatically.
// RegisterDefaults is only needed after custom registry manipulation or extension.
package client

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/app/session"
	internalclient "github.com/openlibrecommunity/olcrtc/internal/client"
	"github.com/openlibrecommunity/olcrtc/internal/control"
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

// HealthStatus is a control-stream health snapshot.
type HealthStatus = control.Status

// HealthFunc is called when the control-stream health snapshot changes.
type HealthFunc func(HealthStatus)

// SessionOpenFunc is called each time a tunnel session is established - on the
// initial connect and after every reconnect - with the server-assigned session
// id. It runs on the connect path and must return promptly.
type SessionOpenFunc func(sessionID string)

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

// Config holds all client tunnel capabilities.
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
	LocalAddr        string
	SOCKSUser        string
	SOCKSPass        string
	DNSServer        string
	Resolver         *net.Resolver
	TransportOptions TransportOptions
	Liveness         LivenessConfig
	Traffic          TrafficConfig
	DeviceID         string
	DeviceIDPath     string
	Claims           map[string]any
	OnHealth         HealthFunc
	OnSessionOpen    SessionOpenFunc
}

type runner func(context.Context, internalclient.Config, func(string)) error

// Client is an embeddable SOCKS5 tunnel client.
type Client struct {
	cfg Config
	run runner
}

// New returns a client configured by cfg.
func New(cfg Config) *Client {
	RegisterDefaults()
	return &Client{cfg: cfg, run: internalclient.RunWithAddress}
}

// Run starts the client and blocks until ctx is canceled or the provider ends.
func (c *Client) Run(ctx context.Context) error {
	return c.RunWithAddress(ctx, nil)
}

// RunWithReady starts the client and calls onReady after the SOCKS listener opens.
func (c *Client) RunWithReady(ctx context.Context, onReady func()) error {
	if onReady == nil {
		return c.RunWithAddress(ctx, nil)
	}
	return c.RunWithAddress(ctx, func(string) { onReady() })
}

// RunWithAddress starts the client and reports the actual SOCKS listener address.
func (c *Client) RunWithAddress(ctx context.Context, onReady func(actualAddr string)) error {
	if err := c.run(ctx, toClientConfig(c.cfg), onReady); err != nil {
		return fmt.Errorf("client: %w", err)
	}
	return nil
}

func toClientConfig(cfg Config) internalclient.Config {
	return internalclient.Config{
		Transport: cfg.Transport, Provider: cfg.Provider, RoomURL: cfg.RoomURL,
		ChannelID: cfg.ChannelID, Engine: cfg.Engine, URL: cfg.URL, Token: cfg.Token,
		ProviderToken: cfg.ProviderToken, KeyHex: cfg.KeyHex, LocalAddr: cfg.LocalAddr,
		SOCKSUser: cfg.SOCKSUser, SOCKSPass: cfg.SOCKSPass, DNSServer: cfg.DNSServer,
		Resolver: cfg.Resolver, TransportOptions: toTransportOptions(cfg.TransportOptions),
		Liveness: control.Config{
			Interval: cfg.Liveness.Interval, Timeout: cfg.Liveness.Timeout, Failures: cfg.Liveness.Failures,
		},
		Traffic: transport.TrafficConfig{
			MaxPayloadSize: cfg.Traffic.MaxPayloadSize,
			MinDelay:       cfg.Traffic.MinDelay, MaxDelay: cfg.Traffic.MaxDelay,
		},
		DeviceID: cfg.DeviceID, DeviceIDPath: cfg.DeviceIDPath, Claims: cfg.Claims,
		OnHealth:      internalclient.HealthFunc(cfg.OnHealth),
		OnSessionOpen: internalclient.SessionOpenFunc(cfg.OnSessionOpen),
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
