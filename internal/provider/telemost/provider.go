// Package telemost implements the Yandex Telemost WebRTC provider.
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

// New creates a new Telemost provider instance.
func New(ctx context.Context, cfg provider.Config) (provider.Provider, error) {
	peer, err := NewPeer(ctx, cfg.RoomURL, cfg.Name, cfg.OnData)
	if err != nil {
		return nil, fmt.Errorf("create telemost peer: %w", err)
	}

	return &telemostProvider{peer: peer}, nil
}

// Connect starts the provider connection.
func (t *telemostProvider) Connect(ctx context.Context) error {
	return t.peer.Connect(ctx)
}

// Send transmits data to the room.
func (t *telemostProvider) Send(data []byte) error {
	return t.peer.Send(data)
}

// Close terminates the provider connection.
func (t *telemostProvider) Close() error {
	return t.peer.Close()
}

// SetReconnectCallback sets the function to call on reconnection.
func (t *telemostProvider) SetReconnectCallback(cb func(*webrtc.DataChannel)) {
	t.peer.SetReconnectCallback(cb)
}

// SetShouldReconnect sets the function to determine if reconnection should occur.
func (t *telemostProvider) SetShouldReconnect(fn func() bool) {
	t.peer.SetShouldReconnect(fn)
}

// SetEndedCallback sets the function to call when the session ends.
func (t *telemostProvider) SetEndedCallback(cb func(string)) {
	t.peer.SetEndedCallback(cb)
}

// WatchConnection monitors the provider connection state.
func (t *telemostProvider) WatchConnection(ctx context.Context) {
	t.peer.WatchConnection(ctx)
}

// CanSend checks if the provider is ready to transmit data.
func (t *telemostProvider) CanSend() bool {
	return t.peer.CanSend()
}

// GetSendQueue returns the data transmission queue.
func (t *telemostProvider) GetSendQueue() chan []byte {
	return t.peer.GetSendQueue()
}

// GetBufferedAmount returns the current WebRTC buffered amount.
func (t *telemostProvider) GetBufferedAmount() uint64 {
	return t.peer.GetBufferedAmount()
}

// AddVideoTrack adds a video track to the telemost connection.
func (t *telemostProvider) AddVideoTrack(track *webrtc.TrackLocalStaticRTP) (*webrtc.RTPSender, error) {
	return t.peer.AddVideoTrack(track)
}

