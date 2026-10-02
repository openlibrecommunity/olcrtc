package client

import (
	"errors"
	"fmt"
	"testing"

	cryptopkg "github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/handshake"
	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
)

// observedLink is a transport that only answers PeerSeen; the classifier
// calls nothing else on it.
type observedLink struct {
	transport.Transport
	seen bool
}

func (l observedLink) PeerSeen() bool    { return l.seen }
func (l observedLink) Send([]byte) error { return nil }
func (l observedLink) CanSend() bool     { return true }

// plainLink has no PeerObserver at all.
type plainLink struct{ transport.Transport }

func (plainLink) Send([]byte) error { return nil }
func (plainLink) CanSend() bool     { return true }

func newClassifyClient(t *testing.T, seen, withObserver bool) (*Client, *muxconn.Conn) {
	t.Helper()
	keys := newClientTestKeys(t)
	var link transport.Transport = observedLink{seen: seen}
	if !withObserver {
		link = plainLink{}
	}
	conn := muxconn.New(link, keys)
	t.Cleanup(func() { _ = conn.Close() })
	return &Client{ln: link, keys: keys}, conn
}

var errHandshakeTimedOut = fmt.Errorf("handshake client: read welcome: %w", errors.New("i/o timeout"))

func TestClassifyBadMagicMeansAnOlderPeer(t *testing.T) {
	c, conn := newClassifyClient(t, true, true)
	conn.Push(make([]byte, cryptopkg.WireOverhead+4))
	if err := c.classifyHandshakeFailure(errHandshakeTimedOut, conn); !errors.Is(err, ErrPeerProtocolMismatch) {
		t.Fatalf("got %v, want ErrPeerProtocolMismatch", err)
	}
}

func TestClassifyAuthFailuresMeanAnotherKey(t *testing.T) {
	c, conn := newClassifyClient(t, true, true)
	other, err := cryptopkg.NewKeySet(make([]byte, 32), cryptopkg.Server)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := other.Seal([]byte("x"), []byte("olcrtc/muxconn/v2/data"))
	if err != nil {
		t.Fatal(err)
	}
	conn.Push(sealed)
	if err := c.classifyHandshakeFailure(errHandshakeTimedOut, conn); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("got %v, want ErrKeyMismatch", err)
	}
}

func TestClassifySilentPeerAndEmptyRoom(t *testing.T) {
	c, conn := newClassifyClient(t, true, true)
	if err := c.classifyHandshakeFailure(errHandshakeTimedOut, conn); !errors.Is(err, ErrPeerSilent) {
		t.Fatalf("seen peer: got %v, want ErrPeerSilent", err)
	}
	c, conn = newClassifyClient(t, false, true)
	if err := c.classifyHandshakeFailure(errHandshakeTimedOut, conn); !errors.Is(err, ErrNoPeer) {
		t.Fatalf("empty room: got %v, want ErrNoPeer", err)
	}
}

func TestClassifyKeepsTheTimeoutWithoutAnObserver(t *testing.T) {
	c, conn := newClassifyClient(t, false, false)
	err := c.classifyHandshakeFailure(errHandshakeTimedOut, conn)
	if errors.Is(err, ErrNoPeer) || errors.Is(err, ErrPeerSilent) || !errors.Is(err, errHandshakeTimedOut) {
		t.Fatalf("got %v, want the original timeout", err)
	}
}

func TestClassifyVersionErrorsAndRejects(t *testing.T) {
	c, conn := newClassifyClient(t, true, true)
	version := fmt.Errorf("handshake: %w", handshake.ErrProtocolVersion)
	if err := c.classifyHandshakeFailure(version, conn); !errors.Is(err, ErrPeerProtocolMismatch) {
		t.Fatalf("version: got %v", err)
	}
	reject := fmt.Errorf("handshake: %w: protocol version mismatch", handshake.ErrRejected)
	if err := c.classifyHandshakeFailure(reject, conn); !errors.Is(err, ErrPeerProtocolMismatch) {
		t.Fatalf("reject: got %v", err)
	}
	auth := fmt.Errorf("handshake: %w: not on the list", handshake.ErrRejected)
	if err := c.classifyHandshakeFailure(auth, conn); errors.Is(err, ErrPeerProtocolMismatch) || !errors.Is(err, handshake.ErrRejected) {
		t.Fatalf("auth reject: got %v, want the reject kept", err)
	}
}
