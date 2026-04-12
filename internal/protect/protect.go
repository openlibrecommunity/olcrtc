// Package protect provides functions to protect sockets from VPN routing.
package protect

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// Protector is called with a socket file descriptor before connect.
// On Android, this calls VpnService.protect(fd) to bypass VPN routing.
var Protector func(fd int) bool //nolint:gochecknoglobals

func controlFunc(network, _ string, c syscall.RawConn) error {
	if Protector == nil {
		return nil
	}
	var err error
	controlErr := c.Control(func(fd uintptr) {
		if !Protector(int(fd)) { //nolint:gosec
			err = &net.OpError{Op: "protect", Net: network, Err: net.ErrClosed}
		}
	})
	if controlErr != nil {
		return fmt.Errorf("control failed: %w", controlErr)
	}
	return err
}

// NewDialer returns a net.Dialer that calls Protector on each new socket.
func NewDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   controlFunc,
	}
}

// NewHTTPClient returns an http.Client using protected sockets.
func NewHTTPClient() *http.Client {
	dialer := NewDialer()
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:  10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	return &http.Client{Transport: transport}
}

// DialContext dials using a protected socket.
func DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := NewDialer().DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("dial failed: %w", err)
	}
	return conn, nil
}

// ProxyDialer implements golang.org/x/net/proxy.Dialer for pion ICE.
type ProxyDialer struct{}

// Dial connects to the address on the named network using a protected socket.
func (d *ProxyDialer) Dial(network, addr string) (net.Conn, error) {
	conn, err := NewDialer().Dial(network, addr)
	if err != nil {
		return nil, fmt.Errorf("dial failed: %w", err)
	}
	return conn, nil
}

// NewProxyDialer returns a proxy.Dialer that protects ICE sockets.
func NewProxyDialer() *ProxyDialer {
	return &ProxyDialer{}
}

