package client

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/logger"
)

const (
	socksVersion                = 5
	socksCmdConnect             = 1
	socksAddrIPv4               = 1
	socksAddrDomain             = 3
	socksAddrIPv6               = 4
	socksRepSuccess             = 0
	socksRepHostUnreachable     = 4
	socksRepCommandNotSupported = 7
)

const (
	// socksNegotiationTimeout bounds everything before the target is known.
	// Until then there is nothing legitimate to wait for, and the listener is
	// allowed to be non-loopback when credentials are configured, so a peer
	// that connects and stays silent must not pin a goroutine and an fd.
	socksNegotiationTimeout = 30 * time.Second

	// maxSocksConns caps concurrent SOCKS clients. Each one costs a
	// goroutine, an fd and a tunnel stream.
	maxSocksConns = 512

	// acceptRetryDelay is the initial backoff after a failed Accept.
	// Retrying immediately turns a temporary fd exhaustion into a hot loop
	// that floods the log.
	acceptRetryDelay    = 10 * time.Millisecond
	maxAcceptRetryDelay = time.Second
)

func (c *Client) acceptLoop(ctx context.Context, listener net.Listener) {
	delay := time.Duration(0)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			delay = nextAcceptDelay(delay)
			logger.Warnf("Accept error (retry in %s): %v", delay, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			continue
		}
		delay = 0
		if !c.registerSocksConn(conn) {
			_ = conn.Close()
			continue
		}
		c.goTracked(func() {
			defer c.unregisterSocksConn(conn)
			c.handleSocks5(ctx, conn)
		})
	}
}

func nextAcceptDelay(current time.Duration) time.Duration {
	if current <= 0 {
		return acceptRetryDelay
	}
	if next := current * 2; next < maxAcceptRetryDelay {
		return next
	}
	return maxAcceptRetryDelay
}

func (c *Client) handleSocks5(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(socksNegotiationTimeout))
	if err := c.socks5Handshake(conn); err != nil {
		return
	}
	targetAddr, targetPort, err := c.socks5Request(conn)
	if err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	if c.peerNoIPv6.Load() && isIPv6Literal(targetAddr) {
		_, _ = conn.Write(replyHostUnreachable(targetAddr))
		return
	}
	const sessionReadyTimeout = 60 * time.Second
	readyCtx, cancel := context.WithTimeout(ctx, sessionReadyTimeout)
	defer cancel()
	for {
		// The ready channel is taken in the same critical section as the
		// state it describes. Sampling it afterwards subscribes to the next
		// generation and misses the signal that just fired, which stalls the
		// request for the full timeout while the tunnel is up.
		session, sessionID, ready := c.sessionSnapshot()
		if session != nil && !session.IsClosed() && sessionID != "" {
			c.tunnel(ctx, conn, session, targetAddr, targetPort)
			return
		}
		select {
		case <-readyCtx.Done():
			_, _ = conn.Write(replyHostUnreachable(targetAddr))
			return
		case <-ready:
		}
	}
}

func (c *Client) socks5Handshake(conn net.Conn) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read socks5 header: %w", err)
	}
	if header[0] != socksVersion {
		return fmt.Errorf("%w: %d", ErrInvalidSOCKSVersion, header[0])
	}
	methods := make([]byte, header[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return fmt.Errorf("read socks5 methods: %w", err)
	}
	if c.socksUser != "" {
		if _, err := conn.Write([]byte{socksVersion, 2}); err != nil {
			return fmt.Errorf("write socks5 auth method: %w", err)
		}
		return c.socks5UserPassAuth(conn)
	}
	if _, err := conn.Write([]byte{socksVersion, 0}); err != nil {
		return fmt.Errorf("write socks5 auth: %w", err)
	}
	return nil
}

func (c *Client) socks5UserPassAuth(conn net.Conn) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read socks5 auth header: %w", err)
	}
	if header[0] != 1 {
		return fmt.Errorf("%w: expected auth version 1, got %d", ErrInvalidSOCKSVersion, header[0])
	}
	user := make([]byte, header[1])
	if _, err := io.ReadFull(conn, user); err != nil {
		return fmt.Errorf("read socks5 username: %w", err)
	}
	passwordLength := make([]byte, 1)
	if _, err := io.ReadFull(conn, passwordLength); err != nil {
		return fmt.Errorf("read socks5 plen: %w", err)
	}
	password := make([]byte, passwordLength[0])
	if _, err := io.ReadFull(conn, password); err != nil {
		return fmt.Errorf("read socks5 password: %w", err)
	}
	// Both comparisons always run: short-circuiting on the username leaks
	// which half failed through timing.
	userOK := subtle.ConstantTimeCompare(user, []byte(c.socksUser)) == 1
	passOK := subtle.ConstantTimeCompare(password, []byte(c.socksPass)) == 1
	if !userOK || !passOK {
		_, _ = conn.Write([]byte{1, 1})
		return ErrSOCKSAuthFailed
	}
	if _, err := conn.Write([]byte{1, 0}); err != nil {
		return fmt.Errorf("write socks5 auth success: %w", err)
	}
	return nil
}

func (c *Client) socks5Request(conn net.Conn) (string, int, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", 0, fmt.Errorf("read socks5 request: %w", err)
	}
	if header[1] != socksCmdConnect {
		// Answer instead of hanging up. The tunnel carries streams, not
		// datagrams, so UDP ASSOCIATE is never going to work; a client that
		// gets a proper refusal reports that and moves on, whereas a dropped
		// connection reads as a broken proxy and invites a retry.
		_, _ = conn.Write(socks5Reply(socksRepCommandNotSupported, ""))
		return "", 0, fmt.Errorf("%w: %d", ErrUnsupportedSOCKSCommand, header[1])
	}
	addr, err := c.readSocks5Addr(conn, header[3])
	if err != nil {
		return "", 0, err
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return "", 0, fmt.Errorf("read socks5 port: %w", err)
	}
	return addr, int(binary.BigEndian.Uint16(portBytes)), nil
}

func (c *Client) readSocks5Addr(conn net.Conn, addrType byte) (string, error) {
	switch addrType {
	case socksAddrIPv4:
		return readIP(conn, net.IPv4len, "ipv4")
	case socksAddrIPv6:
		return readIP(conn, net.IPv6len, "ipv6")
	case socksAddrDomain:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return "", fmt.Errorf("read socks5 domain len: %w", err)
		}
		if length[0] == 0 {
			return "", ErrEmptySOCKSDomain
		}
		buffer := make([]byte, length[0])
		if _, err := io.ReadFull(conn, buffer); err != nil {
			return "", fmt.Errorf("read socks5 domain: %w", err)
		}
		return string(buffer), nil
	default:
		return "", fmt.Errorf("%w: %d", ErrUnsupportedAddressType, addrType)
	}
}

func readIP(conn net.Conn, length int, label string) (string, error) {
	buffer := make([]byte, length)
	if _, err := io.ReadFull(conn, buffer); err != nil {
		return "", fmt.Errorf("read socks5 %s: %w", label, err)
	}
	return net.IP(buffer).String(), nil
}

func socks5Reply(rep byte, target string) []byte {
	addrLen := net.IPv4len
	addrType := byte(socksAddrIPv4)
	if ip := net.ParseIP(target); ip != nil && ip.To4() == nil {
		addrLen = net.IPv6len
		addrType = socksAddrIPv6
	}
	reply := make([]byte, 4+addrLen+2)
	reply[0], reply[1], reply[3] = socksVersion, rep, addrType
	return reply
}

func replySuccess(target string) []byte {
	return socks5Reply(socksRepSuccess, target)
}

func replyHostUnreachable(target string) []byte {
	return socks5Reply(socksRepHostUnreachable, target)
}

// isIPv6Literal reports whether target is an IPv6 address literal. Domain names
// are deliberately excluded: the exit resolves those itself and can pick an A
// record, so short-circuiting them would break hosts that are reachable.
func isIPv6Literal(target string) bool {
	ip := net.ParseIP(target)
	return ip != nil && ip.To4() == nil
}
