package jazz

import (
	"context"
	"fmt"

	"github.com/openlibrecommunity/olcrtc/internal/provider"
	"github.com/pion/webrtc/v4"
)

type jazzProvider struct {
	peer *Peer
}

func New(ctx context.Context, cfg provider.Config) (provider.Provider, error) {
	peer, err := NewPeer(ctx, cfg.Name, cfg.OnData)
	if err != nil {
		return nil, fmt.Errorf("create jazz peer: %w", err)
	}

	return &jazzProvider{peer: peer}, nil
}

func (j *jazzProvider) Connect(ctx context.Context) error {
	return j.peer.Connect(ctx)
}

func (j *jazzProvider) Send(data []byte) error {
	return j.peer.Send(data)
}

func (j *jazzProvider) Close() error {
	return j.peer.Close()
}

func (j *jazzProvider) SetReconnectCallback(cb func(*webrtc.DataChannel)) {
	j.peer.SetReconnectCallback(cb)
}

func (j *jazzProvider) SetShouldReconnect(fn func() bool) {
	j.peer.SetShouldReconnect(fn)
}

func (j *jazzProvider) SetEndedCallback(cb func(string)) {
	j.peer.SetEndedCallback(cb)
}

func (j *jazzProvider) WatchConnection(ctx context.Context) {
	j.peer.WatchConnection(ctx)
}

func (j *jazzProvider) CanSend() bool {
	return j.peer.CanSend()
}

func (j *jazzProvider) GetSendQueue() chan []byte {
	return j.peer.GetSendQueue()
}

func (j *jazzProvider) GetBufferedAmount() uint64 {
	return j.peer.GetBufferedAmount()
}

func init() {
	provider.Register("jazz", New)
}
