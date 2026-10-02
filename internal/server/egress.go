package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/xtaci/smux"

	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
	"github.com/openlibrecommunity/olcrtc/internal/tunnelcore"
)

// socksHandshakeTimeout bounds the upstream SOCKS5 negotiation. The proxy is
// not ours: without a deadline a hung or hostile one parks this goroutine and
// its file descriptor for the process lifetime, and shutdown waits on it.
const socksHandshakeTimeout = 15 * time.Second

// ConnectRequest asks the server to establish a target connection.
type ConnectRequest struct {
	Cmd  string `json:"cmd"`
	Addr string `json:"addr"`
	Port int    `json:"port"`
}

// validate rejects targets that would resolve to something the peer did not
// ask for. An empty host is the important one: net.JoinHostPort("", "80")
// yields ":80", which Go dials as the local machine, so a malformed request
// would turn the exit node into a proxy onto its own loopback.
func (r ConnectRequest) validate() error {
	if r.Addr == "" {
		return fmt.Errorf("%w: empty host", ErrInvalidTarget)
	}
	if r.Port <= 0 || r.Port > 65535 {
		return fmt.Errorf("%w: port %d", ErrInvalidTarget, r.Port)
	}
	return nil
}

func (s *Server) dispatch(ctx context.Context, stream *smux.Stream, request ConnectRequest, sessionID string) {
	addr := net.JoinHostPort(request.Addr, strconv.Itoa(request.Port))
	logger.Infof("sid=%d connect %s", stream.ID(), addr)
	started := time.Now()
	conn, err := s.dial(ctx, request)
	elapsed := time.Since(started)
	if err != nil {
		logger.Infof("sid=%d dial %s failed (%v): %v", stream.ID(), addr, elapsed, err)
		_, _ = stream.Write([]byte{tunnelcore.ConnectAckHostUnreachable})
		return
	}
	defer func() { _ = conn.Close() }()
	logger.Infof("sid=%d connected %s in %v", stream.ID(), addr, elapsed)
	if _, err := stream.Write([]byte{tunnelcore.ConnectAckOK}); err != nil {
		return
	}
	counts, _ := tunnelcore.CopyBidirectional(ctx, stream, conn)
	if s.onTraffic != nil {
		s.onTraffic(sessionID, addr, counts.LeftToRight, counts.RightToLeft)
	}
}

func (s *Server) dial(ctx context.Context, request ConnectRequest) (net.Conn, error) {
	if err := request.validate(); err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(request.Addr, strconv.Itoa(request.Port))
	dialer := protect.NewDialer(s.resolver)
	if s.socksProxyAddr == "" {
		conn, err := dialer.DialContext(ctx, "tcp4", addr)
		if err != nil {
			return nil, fmt.Errorf("dial failed: %w", err)
		}
		return conn, nil
	}
	proxyAddr := net.JoinHostPort(s.socksProxyAddr, strconv.Itoa(s.socksProxyPort))
	conn, err := dialer.DialContext(ctx, "tcp4", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to dial proxy: %w", err)
	}
	if err := s.socks5Connect(conn, request.Addr, request.Port); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func (s *Server) socks5Connect(conn net.Conn, targetAddr string, targetPort int) error {
	_ = conn.SetDeadline(time.Now().Add(socksHandshakeTimeout))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	if err := s.socks5Authenticate(conn); err != nil {
		return err
	}
	if _, err := conn.Write(socks5ConnectRequest(targetAddr, targetPort)); err != nil {
		return fmt.Errorf("failed to write socks5 connect req: %w", err)
	}
	return readSocks5ConnectReply(conn)
}

func socks5ConnectRequest(targetAddr string, targetPort int) []byte {
	request := make([]byte, 0, 4+net.IPv6len+2)
	request = append(request, 5, 1, 0)
	ip := net.ParseIP(targetAddr)
	switch {
	case ip != nil && ip.To4() != nil:
		request = append(request, 1)
		request = append(request, ip.To4()...)
	case ip != nil:
		request = append(request, 4)
		request = append(request, ip.To16()...)
	default:
		if len(targetAddr) > 255 {
			targetAddr = targetAddr[:255]
		}
		request = append(request, 3, byte(len(targetAddr))) //nolint:gosec // length is clamped above
		request = append(request, targetAddr...)
	}
	return append(request, byte(targetPort>>8), byte(targetPort)) //nolint:gosec // wire encoding preserves prior behavior
}

func readSocks5ConnectReply(conn net.Conn) error {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("failed to read socks5 connect resp: %w", err)
	}
	addrLen, err := socks5ReplyAddrLen(conn, header[3])
	if err != nil {
		return err
	}
	if _, err := io.ReadFull(conn, make([]byte, addrLen+2)); err != nil {
		return fmt.Errorf("failed to read socks5 connect resp address: %w", err)
	}
	if header[0] != 5 || header[1] != 0 {
		return fmt.Errorf("%w: %d", ErrSocks5ConnectFailed, header[1])
	}
	return nil
}

func socks5ReplyAddrLen(conn net.Conn, addrType byte) (int, error) {
	switch addrType {
	case 1:
		return net.IPv4len, nil
	case 4:
		return net.IPv6len, nil
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return 0, fmt.Errorf("failed to read socks5 connect resp domain length: %w", err)
		}
		return int(length[0]), nil
	default:
		return 0, fmt.Errorf("%w: address type %d", ErrSocks5ConnectFailed, addrType)
	}
}

func (s *Server) socks5Authenticate(conn net.Conn) error {
	method := byte(0)
	if s.socksProxyUser != "" {
		method = 2
	}
	if _, err := conn.Write([]byte{5, 1, method}); err != nil {
		return fmt.Errorf("failed to write socks5 auth: %w", err)
	}
	response := make([]byte, 2)
	if _, err := io.ReadFull(conn, response); err != nil {
		return fmt.Errorf("failed to read socks5 auth resp: %w", err)
	}
	if response[0] != 5 {
		return ErrSocks5AuthFailed
	}
	switch response[1] {
	case 0:
		if s.socksProxyUser != "" {
			return ErrSocks5AuthFailed
		}
		return nil
	case 2:
		return s.socks5SendCredentials(conn)
	default:
		return ErrSocks5AuthFailed
	}
}

func (s *Server) socks5SendCredentials(conn net.Conn) error {
	user := s.socksProxyUser
	password := s.socksProxyPass
	if len(user) > 255 {
		user = user[:255]
	}
	if len(password) > 255 {
		password = password[:255]
	}
	message := make([]byte, 0, 3+len(user)+len(password))
	message = append(message, 1, byte(len(user))) //nolint:gosec // length is clamped above
	message = append(message, user...)
	message = append(message, byte(len(password))) //nolint:gosec // length is clamped above
	message = append(message, password...)
	if _, err := conn.Write(message); err != nil {
		return fmt.Errorf("failed to write socks5 credentials: %w", err)
	}
	response := make([]byte, 2)
	if _, err := io.ReadFull(conn, response); err != nil {
		return fmt.Errorf("failed to read socks5 credentials resp: %w", err)
	}
	if response[1] != 0 {
		return ErrSocks5AuthFailed
	}
	return nil
}
