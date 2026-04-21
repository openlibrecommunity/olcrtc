// Package link defines link-layer abstractions above transports.
package link

import (
	"context"
	"errors"
)

var (
	// ErrLinkNotFound is returned when a requested link is not registered.
	ErrLinkNotFound = errors.New("link not found")
)

// Link defines a byte link above a transport.
type Link interface {
	Connect(ctx context.Context) error
	Send(data []byte) error
	Close() error
	SetReconnectCallback(cb func())
	SetShouldReconnect(fn func() bool)
	SetEndedCallback(cb func(string))
	WatchConnection(ctx context.Context)
	CanSend() bool
}

// Config holds common link configuration.
type Config struct {
	Transport    string
	Carrier      string
	RoomURL      string
	Name         string
	OnData       func([]byte)
	DNSServer    string
	ProxyAddr    string
	ProxyPort    int
	VideoWidth   int
	VideoHeight  int
	VideoFPS     int
	VideoBitrate string
	VideoHW      string
	VideoQRSize  int
}

// Factory creates a link instance.
type Factory func(ctx context.Context, cfg Config) (Link, error)

var registry = make(map[string]Factory)

// Register adds a link factory to the registry.
func Register(name string, factory Factory) {
	registry[name] = factory
}

// New creates a link instance by name.
func New(ctx context.Context, name string, cfg Config) (Link, error) {
	factory, ok := registry[name]
	if !ok {
		return nil, ErrLinkNotFound
	}
	return factory(ctx, cfg)
}

// Available returns a list of registered link names.
func Available() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	return names
}
