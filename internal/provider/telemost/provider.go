package telemost

import (
	"context"
	"fmt"

	"github.com/openlibrecommunity/olcrtc/internal/provider"
	"github.com/pion/webrtc/v4"
)

type telemostProvider struct {
	peer *Peer
}

func New(ctx context.Context, cfg provider.Config) (provider.Provider, error) {
	peer, err := NewPeer(ctx, cfg.RoomURL, cfg.Name, cfg.OnData)
	if err != nil {
		return nil, fmt.Errorf("create telemost peer: %w", err)
	}

	return &telemostProvider{peer: peer}, nil
}

func (t *telemostProvider) Connect(ctx context.Context) error {
	return t.peer.Connect(ctx)
}

func (t *telemostProvider) Send(data []byte) error {
	return t.peer.Send(data)
}

func (t *telemostProvider) Close() error {
	return t.peer.Close()
}

func (t *telemostProvider) SetReconnectCallback(cb func(*webrtc.DataChannel)) {
	t.peer.SetReconnectCallback(cb)
}

func (t *telemostProvider) SetShouldReconnect(fn func() bool) {
	t.peer.SetShouldReconnect(fn)
}

func (t *telemostProvider) SetEndedCallback(cb func(string)) {
	t.peer.SetEndedCallback(cb)
}

func (t *telemostProvider) WatchConnection(ctx context.Context) {
	t.peer.WatchConnection(ctx)
}

func (t *telemostProvider) CanSend() bool {
	return t.peer.CanSend()
}

func (t *telemostProvider) GetSendQueue() chan []byte {
	return t.peer.GetSendQueue()
}

func (t *telemostProvider) GetBufferedAmount() uint64 {
	return t.peer.GetBufferedAmount()
}

func init() {
	provider.Register("telemost", New)
}
