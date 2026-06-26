package wbstream

import (
	"context"
	"fmt"

	"github.com/openlibrecommunity/olcrtc/internal/auth"
)

// Provider produces LiveKit credentials for the WB Stream service.
type Provider struct{}

// Engine reports which engine consumes credentials from this auth provider.
func (Provider) Engine() string { return "livekit" }

// DefaultServiceURL returns the WB Stream service URL.
func (Provider) DefaultServiceURL() string { return "https://stream.wb.ru" }

// Issue runs the WB Stream auth flow and returns LiveKit credentials.
func (Provider) Issue(ctx context.Context, cfg auth.Config) (auth.Credentials, error) {
	if cfg.RoomURL == "" || cfg.RoomURL == "any" {
		return auth.Credentials{}, auth.ErrRoomIDRequired
	}

	// Owner flow: when an access token is supplied (srv side via yaml engine.token),
	// use it directly so this peer owns the room and keeps it alive. Otherwise register
	// an anonymous guest (cnc side) — guests can only join a room an owner keeps live.
	accessToken := cfg.Token
	if accessToken == "" {
		var err error
		accessToken, err = registerGuest(ctx, cfg.Name)
		if err != nil {
			return auth.Credentials{}, fmt.Errorf("register guest: %w", err)
		}
	}

	roomID := cfg.RoomURL
	if err := joinRoom(ctx, accessToken, roomID); err != nil {
		return auth.Credentials{}, fmt.Errorf("join room: %w", err)
	}

	tok, err := getToken(ctx, accessToken, roomID, cfg.Name)
	if err != nil {
		return auth.Credentials{}, fmt.Errorf("get token: %w", err)
	}

	url := tok.ServerURL
	if url == "" {
		url = defaultWSURL
	}

	return auth.Credentials{
		URL:   url,
		Token: tok.RoomToken,
		Extra: map[string]string{"roomID": roomID},
	}, nil
}

func init() { //nolint:gochecknoinits // auth registration is the canonical Go pattern for plugins
	auth.Register("wbstream", Provider{})
}
