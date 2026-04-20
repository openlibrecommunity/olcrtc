// Package carrier exposes carrier-oriented registration and construction APIs.
package carrier

import (
	"context"
	"errors"

	"github.com/openlibrecommunity/olcrtc/internal/provider"
)

var (
	// ErrCarrierNotFound is returned when a requested carrier is not registered.
	ErrCarrierNotFound = errors.New("carrier not found")
	// ErrByteStreamUnsupported is returned when a carrier cannot provide a byte stream.
	ErrByteStreamUnsupported = errors.New("carrier does not support byte stream")
	// ErrVideoTrackUnsupported is returned when a carrier cannot publish video tracks.
	ErrVideoTrackUnsupported = errors.New("carrier does not support video tracks")
)

// Capabilities describes the transport primitives a carrier can expose.
type Capabilities struct {
	ByteStream bool
	VideoTrack bool
}

// Session is the carrier-level runtime handle.
type Session interface {
	Capabilities() Capabilities
}

// ByteStreamCapable is implemented by carriers that can expose a byte stream.
type ByteStreamCapable interface {
	OpenByteStream() (ByteStream, error)
}

// VideoTrackCapable is implemented by carriers that can publish video tracks.
type VideoTrackCapable interface {
	OpenVideoTrack() (VideoTrack, error)
}

// Config holds carrier configuration.
type Config struct {
	RoomURL   string
	Name      string
	OnData    func([]byte)
	DNSServer string
	ProxyAddr string
	ProxyPort int
}

// Factory creates a new carrier session.
type Factory func(ctx context.Context, cfg Config) (Session, error)

var registry = make(map[string]Factory)

// Register adds a carrier factory to the registry.
func Register(name string, factory Factory) {
	registry[name] = factory
}

// RegisterLegacy adapts an existing provider factory into the carrier registry.
func RegisterLegacy(name string, factory provider.Factory) {
	Register(name, func(ctx context.Context, cfg Config) (Session, error) {
		legacy, err := factory(ctx, provider.Config{
			RoomURL:   cfg.RoomURL,
			Name:      cfg.Name,
			OnData:    cfg.OnData,
			DNSServer: cfg.DNSServer,
			ProxyAddr: cfg.ProxyAddr,
			ProxyPort: cfg.ProxyPort,
		})
		if err != nil {
			return nil, err
		}
		return &legacySession{provider: legacy}, nil
	})
}

// New creates a carrier session by name.
func New(ctx context.Context, name string, cfg Config) (Session, error) {
	factory, ok := registry[name]
	if !ok {
		return nil, ErrCarrierNotFound
	}
	return factory(ctx, cfg)
}

// Available returns a list of registered carriers.
func Available() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	return names
}
