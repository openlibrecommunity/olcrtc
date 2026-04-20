// Package direct provides a pass-through link implementation above transports.
package direct

import (
	"context"
	"fmt"

	"github.com/openlibrecommunity/olcrtc/internal/link"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
)

type directLink struct {
	transport transport.Transport
}

// New creates a direct link that forwards bytes to the selected transport.
func New(ctx context.Context, cfg link.Config) (link.Link, error) {
	tr, err := transport.New(ctx, cfg.Transport, transport.Config{
		Carrier:   cfg.Carrier,
		RoomURL:   cfg.RoomURL,
		Name:      cfg.Name,
		OnData:    cfg.OnData,
		DNSServer: cfg.DNSServer,
		ProxyAddr: cfg.ProxyAddr,
		ProxyPort: cfg.ProxyPort,
	})
	if err != nil {
		return nil, fmt.Errorf("create transport for direct link: %w", err)
	}

	return &directLink{transport: tr}, nil
}

func (d *directLink) Connect(ctx context.Context) error { return d.transport.Connect(ctx) }
func (d *directLink) Send(data []byte) error            { return d.transport.Send(data) }
func (d *directLink) Close() error                      { return d.transport.Close() }
func (d *directLink) SetReconnectCallback(cb func())    { d.transport.SetReconnectCallback(cb) }
func (d *directLink) SetShouldReconnect(fn func() bool) { d.transport.SetShouldReconnect(fn) }
func (d *directLink) SetEndedCallback(cb func(string))  { d.transport.SetEndedCallback(cb) }
func (d *directLink) WatchConnection(ctx context.Context) {
	d.transport.WatchConnection(ctx)
}
func (d *directLink) CanSend() bool { return d.transport.CanSend() }
