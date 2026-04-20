// Package wbstream implements the WB Stream WebRTC provider.
package wbstream

import (
	"context"
	"fmt"

	"github.com/openlibrecommunity/olcrtc/internal/provider"
	"github.com/pion/webrtc/v4"
)

type wbStreamProvider struct {
	peer *Peer
}

// New creates a new WB Stream provider instance.
func New(ctx context.Context, cfg provider.Config) (provider.Provider, error) {
	peer, err := NewPeer(ctx, cfg.RoomURL, cfg.Name, cfg.OnData)
	if err != nil {
		return nil, fmt.Errorf("create wbstream peer: %w", err)
	}

	return &wbStreamProvider{peer: peer}, nil
}

// Connect starts the provider connection.
func (w *wbStreamProvider) Connect(ctx context.Context) error {
	return w.peer.Connect(ctx)
}

// Send transmits data to the room.
func (w *wbStreamProvider) Send(data []byte) error {
	return w.peer.Send(data)
}

// Close terminates the provider connection.
func (w *wbStreamProvider) Close() error {
	return w.peer.Close()
}

// SetReconnectCallback sets the function to call on reconnection.
func (w *wbStreamProvider) SetReconnectCallback(cb func(*webrtc.DataChannel)) {
	w.peer.SetReconnectCallback(cb)
}

// SetShouldReconnect sets the function to determine if reconnection should occur.
func (w *wbStreamProvider) SetShouldReconnect(fn func() bool) {
	w.peer.SetShouldReconnect(fn)
}

// SetEndedCallback sets the function to call when the session ends.
func (w *wbStreamProvider) SetEndedCallback(cb func(string)) {
	w.peer.SetEndedCallback(cb)
}

// WatchConnection monitors the provider connection state.
func (w *wbStreamProvider) WatchConnection(ctx context.Context) {
	w.peer.WatchConnection(ctx)
}

// CanSend checks if the provider is ready to transmit data.
func (w *wbStreamProvider) CanSend() bool {
	return w.peer.CanSend()
}

// GetSendQueue returns the data transmission queue.
func (w *wbStreamProvider) GetSendQueue() chan []byte {
	return w.peer.GetSendQueue()
}

// GetBufferedAmount returns the current WebRTC buffered amount.
func (w *wbStreamProvider) GetBufferedAmount() uint64 {
	return w.peer.GetBufferedAmount()
}

// AddVideoTrack adds a video track to the wbstream connection.
func (w *wbStreamProvider) AddVideoTrack(track *webrtc.TrackLocalStaticRTP) (*webrtc.RTPSender, error) {
	return w.peer.AddVideoTrack(track)
}

