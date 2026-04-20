// Package datachannel provides a transport backed by the current WebRTC providers.
package datachannel

import (
	"context"
	"fmt"

	"github.com/openlibrecommunity/olcrtc/internal/provider"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/pion/webrtc/v4"
)

type providerTransport struct {
	provider provider.Provider
}

// New creates a datachannel transport backed by a carrier-specific provider.
func New(ctx context.Context, cfg transport.Config) (transport.Transport, error) {
	p, err := provider.New(ctx, cfg.Carrier, provider.Config{
		RoomURL:   cfg.RoomURL,
		Name:      cfg.Name,
		OnData:    cfg.OnData,
		DNSServer: cfg.DNSServer,
		ProxyAddr: cfg.ProxyAddr,
		ProxyPort: cfg.ProxyPort,
	})
	if err != nil {
		return nil, fmt.Errorf("create provider transport: %w", err)
	}

	return &providerTransport{provider: p}, nil
}

// Connect starts the transport connection.
func (p *providerTransport) Connect(ctx context.Context) error {
	return p.provider.Connect(ctx)
}

// Send transmits data through the transport.
func (p *providerTransport) Send(data []byte) error {
	return p.provider.Send(data)
}

// Close terminates the transport.
func (p *providerTransport) Close() error {
	return p.provider.Close()
}

// SetReconnectCallback registers reconnect handling.
func (p *providerTransport) SetReconnectCallback(cb func()) {
	p.provider.SetReconnectCallback(func(_ *webrtc.DataChannel) {
		if cb != nil {
			cb()
		}
	})
}

// SetShouldReconnect configures reconnect policy.
func (p *providerTransport) SetShouldReconnect(fn func() bool) {
	p.provider.SetShouldReconnect(fn)
}

// SetEndedCallback registers end-of-session handling.
func (p *providerTransport) SetEndedCallback(cb func(string)) {
	p.provider.SetEndedCallback(cb)
}

// WatchConnection monitors connection lifecycle.
func (p *providerTransport) WatchConnection(ctx context.Context) {
	p.provider.WatchConnection(ctx)
}

// CanSend reports whether transport is ready for sending.
func (p *providerTransport) CanSend() bool {
	return p.provider.CanSend()
}
