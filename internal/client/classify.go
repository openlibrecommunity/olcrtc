package client

import (
	"errors"
	"fmt"
	"strings"

	"github.com/openlibrecommunity/olcrtc/internal/handshake"
	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
)

// Errors a failed handshake is classified into, so an app can tell the user
// what to do rather than that a timeout happened.
var (
	// ErrPeerProtocolMismatch: the peer's records or handshake belong to
	// another protocol version. One side has to update.
	ErrPeerProtocolMismatch = errors.New("peer speaks an incompatible olcrtc protocol")
	// ErrKeyMismatch: the peer's records authenticate under another key.
	ErrKeyMismatch = errors.New("key does not match the peer")
	// ErrPeerSilent: a peer is in the room but never answered the hello.
	// A server on a pre-v2 record layer cannot read it and says nothing.
	ErrPeerSilent = errors.New("peer did not answer the handshake")
	// ErrNoPeer: nothing in the room sent a frame during the handshake.
	ErrNoPeer = errors.New("no peer in room")
)

const rejectVersionReason = "protocol version mismatch"

// classifyHandshakeFailure wraps a handshake error with what the conns and
// the transport saw while it ran. Rejections other than a version mismatch
// are returned as they are: the server answered.
//
// ai-generated: classifier for olcbox's protocol-version message.
func (c *Client) classifyHandshakeFailure(err error, conns ...*muxconn.Conn) error {
	if errors.Is(err, handshake.ErrProtocolVersion) {
		return fmt.Errorf("%w: %w", ErrPeerProtocolMismatch, err)
	}
	if errors.Is(err, handshake.ErrRejected) {
		if strings.Contains(err.Error(), rejectVersionReason) {
			return fmt.Errorf("%w: %w", ErrPeerProtocolMismatch, err)
		}
		return err
	}
	var stats muxconn.DecryptStats
	for _, conn := range conns {
		if conn == nil {
			continue
		}
		s := conn.DecryptStats()
		stats.BadMagic += s.BadMagic
		stats.AuthFailed += s.AuthFailed
	}
	switch {
	case stats.BadMagic > 0:
		return fmt.Errorf("%w: %w", ErrPeerProtocolMismatch, err)
	case stats.AuthFailed > 0:
		return fmt.Errorf("%w: %w", ErrKeyMismatch, err)
	}
	observer, ok := c.ln.(transport.PeerObserver)
	if !ok {
		return err
	}
	if observer.PeerSeen() {
		return fmt.Errorf("%w: %w", ErrPeerSilent, err)
	}
	return fmt.Errorf("%w: %w", ErrNoPeer, err)
}
