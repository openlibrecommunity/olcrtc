// Package jazz implements the SaluteJazz WebRTC provider.
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

// New creates a new SaluteJazz provider instance.
func New(ctx context.Context, cfg provider.Config) (provider.Provider, error) {
	peer, err := NewPeer(ctx, cfg.RoomURL, cfg.Name, cfg.OnData)
	if err != nil {
		return nil, fmt.Errorf("create jazz peer: %w", err)
	}

	return &jazzProvider{peer: peer}, nil
}

// Connect starts the provider connection.
func (j *jazzProvider) Connect(ctx context.Context) error {
	return j.peer.Connect(ctx)
}

// Send transmits data to the room.
func (j *jazzProvider) Send(data []byte) error {
	return j.peer.Send(data)
}

// Close terminates the provider connection.
func (j *jazzProvider) Close() error {
	return j.peer.Close()
}

// SetReconnectCallback sets the function to call on reconnection.
func (j *jazzProvider) SetReconnectCallback(cb func(*webrtc.DataChannel)) {
	j.peer.SetReconnectCallback(cb)
}

// SetShouldReconnect sets the function to determine if reconnection should occur.
func (j *jazzProvider) SetShouldReconnect(fn func() bool) {
	j.peer.SetShouldReconnect(fn)
}

// SetEndedCallback sets the function to call when the session ends.
func (j *jazzProvider) SetEndedCallback(cb func(string)) {
	j.peer.SetEndedCallback(cb)
}

// WatchConnection monitors the provider connection state.
func (j *jazzProvider) WatchConnection(ctx context.Context) {
	j.peer.WatchConnection(ctx)
}

// CanSend checks if the provider is ready to transmit data.
func (j *jazzProvider) CanSend() bool {
	return j.peer.CanSend()
}

// GetSendQueue returns the data transmission queue.
func (j *jazzProvider) GetSendQueue() chan []byte {
	return j.peer.GetSendQueue()
}

// GetBufferedAmount returns the current WebRTC buffered amount.
func (j *jazzProvider) GetBufferedAmount() uint64 {
	return j.peer.GetBufferedAmount()
}

// AddVideoTrack adds a video track to the jazz connection.
func (j *jazzProvider) AddVideoTrack(track *webrtc.TrackLocalStaticRTP) (*webrtc.RTPSender, error) {
	return j.peer.AddVideoTrack(track)
}
