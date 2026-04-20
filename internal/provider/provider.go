// Package provider defines the interface and registry for different WebRTC providers.
package provider

import (
	"context"
	"errors"

	"github.com/pion/webrtc/v4"
)

var (
	// ErrProviderNotFound is returned when a requested provider is not registered.
	ErrProviderNotFound = errors.New("provider not found")
	// ErrDataChannelTimeout is returned when the DataChannel fails to open within the timeout period.
	ErrDataChannelTimeout = errors.New("datachannel timeout")
	// ErrDataChannelNotReady is returned when attempting to send data before the DataChannel is open.
	ErrDataChannelNotReady = errors.New("datachannel not ready")
	// ErrSendQueueClosed is returned when attempting to send data after the send queue has been closed.
	ErrSendQueueClosed = errors.New("send queue closed")
	// ErrSendQueueTimeout is returned when the send queue is full and the timeout is reached.
	ErrSendQueueTimeout = errors.New("send queue timeout")
)

// Provider defines the standard interface for WebRTC connection handlers.
//
//nolint:interfacebloat // All methods are necessary for provider abstraction.
type Provider interface {
	Connect(ctx context.Context) error
	Send(data []byte) error
	Close() error
	SetReconnectCallback(cb func(*webrtc.DataChannel))
	SetShouldReconnect(fn func() bool)
	SetEndedCallback(cb func(string))
	WatchConnection(ctx context.Context)
	CanSend() bool
	GetSendQueue() chan []byte
	GetBufferedAmount() uint64
}

// VideoTrackCapable is implemented by providers that can publish video tracks.
type VideoTrackCapable interface {
	AddVideoTrack(track *webrtc.TrackLocalStaticRTP) (*webrtc.RTPSender, error)
}

// Config holds common configuration for all providers.
type Config struct {
	RoomURL   string
	Name      string
	OnData    func([]byte)
	DNSServer string
	ProxyAddr string
	ProxyPort int
}

// Factory is a function that creates a new Provider instance.
type Factory func(ctx context.Context, cfg Config) (Provider, error)

// registry holds all registered provider factories.
//
//nolint:gochecknoglobals // Global registry is required for provider discovery.
var registry = make(map[string]Factory)

// Register adds a new provider factory to the registry.
func Register(name string, factory Factory) {
	registry[name] = factory
}

// New creates a new Provider instance by name.
func New(ctx context.Context, name string, cfg Config) (Provider, error) {
	factory, ok := registry[name]
	if !ok {
		return nil, ErrProviderNotFound
	}
	return factory(ctx, cfg)
}

// Available returns a list of registered provider names.
func Available() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	return names
}
