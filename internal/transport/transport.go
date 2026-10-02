// Package transport defines transport abstractions and registry.
//
// A transport encodes byte payloads onto a provider (engine) primitive - either
// a reliable byte stream (datachannel) or a video track (videochannel,
// seichannel, vp8channel). Transport-specific tuning lives in per-transport
// Options types; the common configuration shared by every transport lives in
// [Config].
package transport

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/engine"
	enginebuiltin "github.com/openlibrecommunity/olcrtc/internal/engine/builtin"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

// ErrTransportNotFound is returned when a requested transport is not registered.
var ErrTransportNotFound = errors.New("transport not found")

// ErrOptionsTypeMismatch is returned when a transport receives options of the wrong type.
var ErrOptionsTypeMismatch = errors.New("transport options type mismatch")

// ErrPeerIdentityUnsupported is returned when a transport cannot confirm routing peers.
var ErrPeerIdentityUnsupported = errors.New("peer identity unsupported")

// ErrInvalidPeerID is returned when an authenticated routing peer ID is malformed.
var ErrInvalidPeerID = errors.New("invalid peer id")

// Features describes the delivery semantics of a transport.
//
// It used to also advertise Reliable/Ordered/MessageOriented. All four
// transports hardcoded them to true - including the ack-based ones, where
// "reliable" means a bounded number of retransmit attempts and a Send that
// can still fail - and nothing ever read them. Rather than keep three
// unread booleans honest, they are gone; the payload cap is the one property
// upper layers actually size their frames against (see runtime.MaxPayload).
type Features struct {
	MaxPayloadSize int
}

// Transport defines a byte transport independent of the underlying provider.
type Transport interface {
	Connect(ctx context.Context) error
	Send(data []byte) error
	Close() error
	SetReconnectCallback(cb func())
	SetShouldReconnect(fn func() bool)
	SetEndedCallback(cb func(string))
	WatchConnection(ctx context.Context)
	CanSend() bool
	Features() Features
	// Reconnect asks the underlying provider (engine) to tear down and
	// re-establish the SFU connection. Upper layers call this when a
	// liveness probe declares the link dead - useful when the engine has
	// not yet noticed silent packet loss.
	Reconnect(reason string)
}

// ControlPlane is implemented by transports that can route control-plane
// traffic independently of the bulk data plane. When a transport implements
// this interface, callers should use ControlSend/ControlOnData for the first
// smux stream (the olcrtc control/handshake stream) so that it does not
// compete with bulk data in the same KCP send buffer.
type ControlPlane interface {
	// ControlSend sends a raw encrypted frame on the control-plane channel.
	ControlSend(data []byte) error
	// SetControlOnData registers the callback invoked for every frame
	// received on the control-plane channel.
	SetControlOnData(cb func([]byte))
	// ControlCanSend reports whether the control-plane is ready to send.
	// Unlike CanSend, this should return true as soon as the subscriber PC
	// is connected - it does NOT require the publisher PC to be ready.
	ControlCanSend() bool
}

// PeerTransport is implemented by transports whose provider can identify and
// address individual remote endpoints.
type PeerTransport interface {
	Transport
	SendTo(peerID string, data []byte) error
	SupportsPeerRouting() bool
}

// PeerControlPlane is implemented by transports that support per-peer isolated
// control planes. Each peer identified by peerID gets its own KCP session so
// that multiple clients can handshake and maintain liveness independently.
// The server uses this to create per-peer smux control sessions.
type PeerControlPlane interface {
	// ControlSendTo sends a control frame to a specific peer.
	ControlSendTo(peerID string, data []byte) error
	// SetControlOnPeerData registers the callback invoked when a control frame
	// arrives for any peer. peerID is the hex data-epoch string.
	SetControlOnPeerData(cb func(peerID string, data []byte))
	// ControlPeerCanSend reports whether the control plane for a specific peer
	// is ready to send.
	ControlPeerCanSend(peerID string) bool
}

// PeerReadyTransport is implemented by transports whose provider can signal
// when a remote peer has appeared. WaitForPeer blocks until the remote side
// is confirmed ready (first epoch frame received), or ctx is cancelled.
type PeerReadyTransport interface {
	WaitForPeer(ctx context.Context) error
}

// PeerIdentity is implemented by transports that authenticate a routing peer
// through the encrypted handshake before accepting its data-plane frames.
type PeerIdentity interface {
	LocalPeerID() string
	ConfirmPeer(peerID string) error
}

// LinkHealthObserver is implemented by transports whose peer-restart
// heuristics want corroborating evidence from a session-specific liveness
// signal before acting on provider-level noise (e.g. unrelated room
// participants).
type LinkHealthObserver interface {
	NotifyLinkHealth(unhealthy bool)
}

// PeerResetter is implemented by transports (and engines) that latch onto a
// single remote peer and can be told to forget it, so the next frame from any
// peer re-latches. Used when a session is torn down and rebuilt.
type PeerResetter interface {
	ResetPeer()
}

// Options is a marker for per-transport option structs. Each transport package
// defines its own Options type (e.g. videochannel.Options) and registers a
// factory that consumes it via type assertion. A nil Options is valid for
// transports that need no extra configuration (e.g. datachannel).
type Options interface {
	TransportOptions()
}

// OptionsAs extracts the per-transport options from cfg. A nil Options
// yields the zero value of T, which every transport turns into its documented
// defaults via withDefaults. name identifies the transport in the error.
func OptionsAs[T Options](cfg Config, name string) (T, error) {
	var zero T

	if cfg.Options == nil {
		return zero, nil
	}

	opts, ok := cfg.Options.(T)
	if !ok {
		return zero, fmt.Errorf("%w: %s: got %T", ErrOptionsTypeMismatch, name, cfg.Options)
	}

	return opts, nil
}

// MaxFPS bounds the frame rate every video transport derives its writer tick
// from. time.Second/FPS truncates to zero above one billion, and a zero tick
// panics time.NewTicker and divides by zero in the keepalive arithmetic - both
// inside a writer goroutine, where the panic is unrecoverable. No provider
// carries anything near this rate anyway.
const MaxFPS = 240

// NormalizeFPS clamps fps into (0, MaxFPS], substituting def when it is unset.
func NormalizeFPS(fps, def int) int {
	if fps <= 0 {
		return def
	}
	if fps > MaxFPS {
		return MaxFPS
	}
	return fps
}

// TrafficConfig controls optional reliability-oriented send shaping.
type TrafficConfig struct {
	MaxPayloadSize int
	MinDelay       time.Duration
	MaxDelay       time.Duration
}

// Config holds common transport configuration applicable to every transport.
type Config struct {
	// Provider is the auth-provider name; engine/URL/token are resolved through it.
	Provider string
	RoomURL  string
	// Engine, URL, Token are forwarded to provider.Config for the "none" auth
	// provider (direct engine access without a service-specific auth flow).
	Engine string
	URL    string
	Token  string
	// ProviderToken is an optional pre-issued account token forwarded to the auth
	// provider (e.g. a WB Stream account token). Empty uses the provider's
	// default guest flow.
	ProviderToken string
	ChannelID     string
	DeviceID      string
	Name          string
	OnData        func([]byte)
	OnPeerData    func(peerID string, data []byte)
	DNSServer     string
	Resolver      protect.Lookup
	ProxyAddr     string
	ProxyPort     int

	// RequireTargetedPeer makes single-peer engines ignore broadcast frames
	// from unrelated olcrtc clients until a peer sends a frame addressed to
	// this session's local epoch. Server-side transports leave this disabled
	// so they can accept initial broadcast CLIENT_HELLO frames.
	RequireTargetedPeer bool

	// Options carries transport-specific tuning. Type is per-transport-package.
	Options Options

	// Traffic controls payload-size and pacing shaping applied around the
	// underlying transport's Send.
	Traffic TrafficConfig
}

// EngineConfig projects the provider-facing part of the transport config onto
// the engine builder config, so every transport opens its engine session the
// same way instead of copying the field list by hand.
func (c Config) EngineConfig() enginebuiltin.Config {
	return enginebuiltin.Config{
		RoomURL:             c.RoomURL,
		Name:                c.Name,
		OnData:              c.OnData,
		OnPeerData:          c.OnPeerData,
		DNSServer:           c.DNSServer,
		Resolver:            c.Resolver,
		ProxyAddr:           c.ProxyAddr,
		ProxyPort:           c.ProxyPort,
		RequireTargetedPeer: c.RequireTargetedPeer,
		Engine:              c.Engine,
		URL:                 c.URL,
		Token:               c.Token,
		ProviderToken:       c.ProviderToken,
	}
}

// OpenEngine resolves the configured provider and opens an engine session.
func (c Config) OpenEngine(ctx context.Context) (engine.Session, error) {
	sess, err := enginebuiltin.Open(ctx, c.Provider, c.EngineConfig())
	if err != nil {
		return nil, fmt.Errorf("open engine session: %w", err)
	}

	return sess, nil
}

// Factory creates a transport instance.
type Factory func(ctx context.Context, cfg Config) (Transport, error)

//nolint:gochecknoglobals // process-wide transport registry
var (
	registryMu sync.RWMutex
	registry   = make(map[string]Factory)
)

// Register adds a transport factory to the registry.
func Register(name string, factory Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()

	registry[name] = factory
}

// New creates a transport instance by name.
func New(ctx context.Context, name string, cfg Config) (Transport, error) {
	registryMu.RLock()
	factory, ok := registry[name]
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrTransportNotFound, name)
	}

	return factory(ctx, cfg)
}

// Available returns the sorted list of registered transport names.
func Available() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}
