// Package tunnelcore implements shared tunnel session infrastructure.
package tunnelcore

import (
	"fmt"
	"net"
	"time"

	"github.com/xtaci/smux"

	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
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
//
// Best-effort, but not silent: this frame is what lets a peer fail over the
// instant we leave instead of waiting out its liveness window, so a failure to
// deliver it is the difference between a seamless handover and a minute of dead
// tunnel. Log it rather than swallowing it.
func NotifyControlClose(stream *smux.Stream) {
	if stream == nil {
		return
	}
	_ = stream.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := SendControlClose(stream); err != nil {
		logger.Warnf("control close NOT delivered: %v", err)
	} else {
		time.Sleep(200 * time.Millisecond)
	}
	_ = stream.SetWriteDeadline(time.Time{})
	_ = stream.CloseWrite()
}
