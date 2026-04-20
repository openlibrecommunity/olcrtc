package carrier

import (
	"context"

	"github.com/pion/webrtc/v4"
)

// ByteStream is a carrier capability for bidirectional byte transport.
type ByteStream interface {
	Connect(ctx context.Context) error
	Send(data []byte) error
	Close() error
	SetReconnectCallback(cb func())
	SetShouldReconnect(fn func() bool)
	SetEndedCallback(cb func(string))
	WatchConnection(ctx context.Context)
	CanSend() bool
}

type providerByteStream struct {
	carrier Carrier
}

// OpenByteStream adapts a carrier to a generic byte-stream capability.
func OpenByteStream(c Carrier) ByteStream {
	return &providerByteStream{carrier: c}
}

func (p *providerByteStream) Connect(ctx context.Context) error { return p.carrier.Connect(ctx) }
func (p *providerByteStream) Send(data []byte) error            { return p.carrier.Send(data) }
func (p *providerByteStream) Close() error                      { return p.carrier.Close() }

func (p *providerByteStream) SetReconnectCallback(cb func()) {
	p.carrier.SetReconnectCallback(func(_ *webrtc.DataChannel) {
		if cb != nil {
			cb()
		}
	})
}

func (p *providerByteStream) SetShouldReconnect(fn func() bool) { p.carrier.SetShouldReconnect(fn) }
func (p *providerByteStream) SetEndedCallback(cb func(string))  { p.carrier.SetEndedCallback(cb) }
func (p *providerByteStream) WatchConnection(ctx context.Context) {
	p.carrier.WatchConnection(ctx)
}
func (p *providerByteStream) CanSend() bool { return p.carrier.CanSend() }
