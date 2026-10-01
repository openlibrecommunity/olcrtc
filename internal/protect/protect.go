// Package protect provides functions to protect sockets from VPN routing.
package protect

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultDialTimeout       = 10 * time.Second
	defaultKeepAlive         = 30 * time.Second
	defaultIdleConnTimeout   = 30 * time.Second
	defaultTLSHandshake      = 10 * time.Second
	defaultResponseHeader    = 10 * time.Second
	defaultWebSocketTimeout  = 10 * time.Second
	defaultHTTPClientTimeout = 30 * time.Second
	defaultStatusBodyLimit   = 1024
)

var (
	errRequestBodyNotReplayable = errors.New("request body is not replayable")
	sensitiveFieldRE            = regexp.MustCompile(
		`(?i)((?:access[_-]?token|room[_-]?token|token|credentials)"?\s*[:=]\s*"?)` +
			`[^",\s}]+`,
	)
	sensitiveBearerRE = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`)
)

// ErrProtectorRejected reports that the host protector refused a socket.
//
// This used to surface as net.ErrClosed, which prints "use of closed network
// connection" - a protector that fails only for AF_INET6 then reads as an
// unrelated bug in whatever was dialing. The network is in the OpError, so the
// message now names both the cause and the family it happened on.
var ErrProtectorRejected = errors.New("socket protector rejected the socket")

type protectorHolder struct {
	protect func(int) bool
}

//nolint:gochecknoglobals // Android VpnService socket protection is process-wide by kernel fd
var protector atomic.Pointer[protectorHolder]

// SetProtector sets the process-wide Android VpnService socket callback.
func SetProtector(protectFunc func(int) bool) {
	if protectFunc == nil {
		protector.Store(nil)
		return
	}
	protector.Store(&protectorHolder{protect: protectFunc})
}

// HasProtector reports whether Android socket protection is configured.
func HasProtector() bool {
	return protector.Load() != nil
}

func controlFunc(network, _ string, c syscall.RawConn) error {
	current := protector.Load()
	if current == nil {
		return nil
	}
	var err error
	controlErr := c.Control(func(fd uintptr) {
		if !current.protect(int(fd)) {
			err = &net.OpError{Op: "protect", Net: network, Err: ErrProtectorRejected}
		}
	})
	if controlErr != nil {
		return fmt.Errorf("control failed: %w", controlErr)
	}
	return err
}

// newTLSConfig returns the shared TLS policy for provider HTTP/WebSocket clients.
func newTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

// newHTTPTransport returns an HTTP transport using protected sockets and sane timeouts.
func newHTTPTransport(lookups ...Lookup) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           NewDialer(lookups...).DialContext,
		TLSClientConfig:       newTLSConfig(),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       defaultIdleConnTimeout,
		TLSHandshakeTimeout:   defaultTLSHandshake,
		ResponseHeaderTimeout: defaultResponseHeader,
	}
}

// NewHTTPClient returns an http.Client using protected sockets with DNS retry.
// Names resolve through the first non-nil lookup.
func NewHTTPClient(lookups ...Lookup) *http.Client {
	return &http.Client{
		Transport: &retryTransport{base: newHTTPTransport(lookups...)},
		Timeout:   defaultHTTPClientTimeout,
	}
}

// retryTransport retries requests on transient DNS/dial errors.
type retryTransport struct {
	base http.RoundTripper
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	const maxRetries = 3
	var resp *http.Response
	var err error
	for i := range maxRetries {
		if waitErr := waitForRetry(req.Context(), i); waitErr != nil {
			return nil, waitErr
		}
		attempt, requestErr := requestForAttempt(req, i)
		if requestErr != nil {
			return resp, fmt.Errorf("prepare retry: %w", requestErr)
		}
		resp, err = t.base.RoundTrip(attempt)
		if err == nil || !isRetriableError(err) {
			if err != nil {
				return resp, fmt.Errorf("round trip: %w", err)
			}
			return resp, nil
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	return resp, fmt.Errorf("round trip after %d retries: %w", maxRetries, err)
}

func waitForRetry(ctx context.Context, attempt int) error {
	if attempt == 0 {
		return nil
	}
	timer := time.NewTimer(time.Duration(attempt) * 500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("retry wait: %w", ctx.Err())
	}
}

func requestForAttempt(req *http.Request, attempt int) (*http.Request, error) {
	if attempt == 0 {
		return req, nil
	}
	retry := req.Clone(req.Context())
	if req.Body == nil {
		return retry, nil
	}
	if req.GetBody == nil {
		return nil, errRequestBodyNotReplayable
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("recreate request body: %w", err)
	}
	retry.Body = body
	return retry, nil
}

func isRetriableError(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	// No route is a statement about this instant, not about the host: the
	// link is still coming up, the phone is mid-handover, or the socket was
	// pinned to an interface that had nothing behind it. It used to be the one
	// dial error that ended the request on the first try, which on a mobile
	// carrier is the difference between connecting and not.
	if errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Timeout() || strings.Contains(opErr.Error(), "connection refused")
	}
	s := err.Error()
	return strings.Contains(s, "no such host") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "i/o timeout")
}

// NewWebSocketDialer returns a WebSocket dialer using protected sockets and shared TLS policy.
func NewWebSocketDialer(handshakeTimeout time.Duration, lookups ...Lookup) websocket.Dialer {
	if handshakeTimeout <= 0 {
		handshakeTimeout = defaultWebSocketTimeout
	}
	return websocket.Dialer{
		NetDialContext:   NewDialer(lookups...).DialContext,
		Proxy:            http.ProxyFromEnvironment,
		TLSClientConfig:  newTLSConfig(),
		HandshakeTimeout: handshakeTimeout,
	}
}

// StatusError formats an upstream HTTP error while bounding and redacting the body.
func StatusError(base error, resp *http.Response, limit int64) error {
	if limit <= 0 {
		limit = defaultStatusBodyLimit
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
	bodyText := redactSensitive(strings.TrimSpace(string(body)))
	if bodyText == "" {
		return fmt.Errorf("%w: status %d", base, resp.StatusCode)
	}
	return fmt.Errorf("%w: status %d: %s", base, resp.StatusCode, bodyText)
}

// redactSensitive removes common token-like values from provider error text.
func redactSensitive(text string) string {
	text = sensitiveBearerRE.ReplaceAllString(text, "${1}<redacted>")
	return sensitiveFieldRE.ReplaceAllString(text, "${1}<redacted>")
}

// ProxyDialer implements golang.org/x/net/proxy.Dialer for pion ICE.
type ProxyDialer struct {
	dialer *Dialer
}

// Dial connects to the address on the named network using a protected socket.
func (d *ProxyDialer) Dial(network, addr string) (net.Conn, error) {
	return d.dialer.Dial(network, addr)
}

// NewProxyDialer returns a proxy.Dialer that protects ICE sockets.
func NewProxyDialer(lookups ...Lookup) *ProxyDialer {
	return &ProxyDialer{dialer: NewDialer(lookups...)}
}
