// Package tunnelcore implements shared tunnel session infrastructure.
package tunnelcore

import (
	"fmt"
	"net"
	"time"

	"github.com/xtaci/smux"

	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
)

// Tunnel CONNECT reply bytes are SOCKS5 REP values sent before any payload.
const (
	ConnectAckOK              byte = 0x00
	ConnectAckHostUnreachable byte = 0x04
)

// SetupKeySet creates shared directional tunnel keys and adds the role to errors.
func SetupKeySet(keyHex string, role crypto.Role) (*crypto.KeySet, error) {
	keys, err := runtime.SetupKeySet(keyHex, role)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", role, err)
	}
	return keys, nil
}

// SetupMultiKeySet creates shared directional tunnel keys for multiple key hexes.
// For backward compatibility, if only one key is provided, returns a MultiKeySet
// that wraps a single KeySet. This allows the server to support fallback decryption
// with multiple keys.
func SetupMultiKeySet(keyHexes []string, role crypto.Role) (*crypto.MultiKeySet, error) {
	if len(keyHexes) == 0 {
		return nil, fmt.Errorf("%s: %w", role, runtime.ErrKeyRequired)
	}
	keys, err := crypto.NewMultiKeySet(keyHexes, role)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", role, err)
	}
	return keys, nil
}

// Resolver returns the supplied resolver or a protected resolver for dnsServer.
func Resolver(resolver *net.Resolver, dnsServer string) *net.Resolver {
	if resolver != nil {
		return resolver
	}
	return protect.NewResolver(dnsServer)
}

// PushData forwards one transport frame to conn when a session is installed.
func PushData(conn *muxconn.Conn, data []byte) {
	if conn != nil {
		conn.Push(data)
	}
}

// ResetPeer asks transports that latch a peer epoch to forget it.
func ResetPeer(tr transport.Transport) {
	if resetter, ok := tr.(transport.PeerResetter); ok {
		resetter.ResetPeer()
	}
}

// NotifyControlClose sends a bounded best-effort graceful close notification.
func NotifyControlClose(stream *smux.Stream) {
	if stream == nil {
		return
	}
	_ = stream.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := SendControlClose(stream); err == nil {
		time.Sleep(200 * time.Millisecond)
	}
	_ = stream.SetWriteDeadline(time.Time{})
	_ = stream.CloseWrite()
}
