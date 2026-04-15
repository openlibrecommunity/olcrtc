// Package jazzvisual exposes the Jazz signaling stack with the future visual
// media transport selected instead of the legacy DataChannel path.
package jazzvisual

import (
	"context"
	"fmt"

	"github.com/openlibrecommunity/olcrtc/internal/provider"
	"github.com/openlibrecommunity/olcrtc/internal/provider/jazz"
	"github.com/pion/webrtc/v4"
)

type jazzVisualProvider struct {
	peer *jazz.Peer
}

// New creates a new Jazz provider instance backed by the visual media engine.
func New(ctx context.Context, cfg provider.Config) (provider.Provider, error) {
	peer, err := jazz.NewVisualPeer(ctx, cfg.RoomURL, cfg.Name, cfg.OnData)
	if err != nil {
		return nil, fmt.Errorf("create jazz-visual peer: %w", err)
	}

	return &jazzVisualProvider{peer: peer}, nil
}

func (j *jazzVisualProvider) Connect(ctx context.Context) error {
	if err := j.peer.Connect(ctx); err != nil {
		return fmt.Errorf("connect jazz-visual peer: %w", err)
	}
	return nil
}

func (j *jazzVisualProvider) Send(data []byte) error {
	if err := j.peer.Send(data); err != nil {
		return fmt.Errorf("send via jazz-visual peer: %w", err)
	}
	return nil
}

func (j *jazzVisualProvider) Close() error {
	if err := j.peer.Close(); err != nil {
		return fmt.Errorf("close jazz-visual peer: %w", err)
	}
	return nil
}

func (j *jazzVisualProvider) SetReconnectCallback(cb func(*webrtc.DataChannel)) {
	j.peer.SetReconnectCallback(cb)
}

func (j *jazzVisualProvider) SetShouldReconnect(fn func() bool) { j.peer.SetShouldReconnect(fn) }

func (j *jazzVisualProvider) SetEndedCallback(cb func(string)) { j.peer.SetEndedCallback(cb) }

func (j *jazzVisualProvider) WatchConnection(ctx context.Context) { j.peer.WatchConnection(ctx) }

func (j *jazzVisualProvider) CanSend() bool { return j.peer.CanSend() }

func (j *jazzVisualProvider) GetSendQueue() chan []byte { return j.peer.GetSendQueue() }

func (j *jazzVisualProvider) GetBufferedAmount() uint64 { return j.peer.GetBufferedAmount() }
