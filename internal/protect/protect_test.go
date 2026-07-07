package protect

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

var errProtectBoom = errors.New("boom")

type rawConnStub struct {
	controlFn func(func(uintptr)) error
}

func (r rawConnStub) Control(fn func(uintptr)) error {
	if r.controlFn != nil {
		return r.controlFn(fn)
	}
	fn(42)
	return nil
}
func (r rawConnStub) Read(func(uintptr) bool) error  { return nil }
func (r rawConnStub) Write(func(uintptr) bool) error { return nil }

func TestControlFuncWithoutProtector(t *testing.T) {
	old := Protector
	Protector = nil
	t.Cleanup(func() { Protector = old })

	if err := controlFunc("tcp4", "", rawConnStub{}); err != nil {
		t.Fatalf("controlFunc() error = %v", err)
	}
}

func TestControlFuncWithProtector(t *testing.T) {
	old := Protector
	t.Cleanup(func() { Protector = old })

	called := 0
	Protector = func(fd int) bool {
		called++
		if fd != 42 {
			t.Fatalf("Protector fd = %d, want 42", fd)
		}
		return true
	}
	if err := controlFunc("tcp4", "", rawConnStub{}); err != nil {
		t.Fatalf("controlFunc() error = %v", err)
	}
	if called != 1 {
		t.Fatalf("Protector calls = %d, want 1", called)
	}

	Protector = func(int) bool { return false }
	err := controlFunc("tcp4", "", rawConnStub{})
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "protect" {
		t.Fatalf("controlFunc() error = %v, want protect op error", err)
	}
}

func TestControlFuncWrapsControlError(t *testing.T) {
	old := Protector
	Protector = func(int) bool { return true }
	t.Cleanup(func() { Protector = old })

	err := controlFunc("tcp4", "", rawConnStub{
		controlFn: func(func(uintptr)) error { return errProtectBoom },
	})
	if err == nil || err.Error() != "control failed: boom" {
		t.Fatalf("controlFunc() error = %v", err)
	}
}

//nolint:cyclop // table-driven test naturally has many branches
func TestNewDialerAndHTTPClient(t *testing.T) {
	dialer := NewDialer()
	if dialer.Timeout != 10*time.Second || dialer.KeepAlive != 30*time.Second || dialer.Control == nil {
		t.Fatalf("NewDialer() = %+v", dialer)
	}

	client := NewHTTPClient()
	rt, ok := client.Transport.(*retryTransport)
	if !ok {
		t.Fatalf("Transport type = %T, want *protect.retryTransport", client.Transport)
	}
	tr, ok := rt.base.(*http.Transport)
	if !ok {
		t.Fatalf("base Transport type = %T, want *http.Transport", rt.base)
	}
	if tr.Proxy == nil || tr.DialContext == nil || tr.TLSClientConfig == nil ||
		tr.TLSClientConfig.MinVersion != tls.VersionTLS12 || !tr.ForceAttemptHTTP2 || tr.MaxIdleConns != 10 ||
		tr.IdleConnTimeout != 30*time.Second || tr.TLSHandshakeTimeout != 10*time.Second ||
		tr.ResponseHeaderTimeout != 10*time.Second || client.Timeout != 30*time.Second {
		t.Fatalf("transport = %+v", tr)
	}
}

func TestNewWebSocketDialer(t *testing.T) {
	dialer := NewWebSocketDialer(3 * time.Second)
	if dialer.NetDialContext == nil || dialer.Proxy == nil || dialer.TLSClientConfig == nil ||
		dialer.TLSClientConfig.MinVersion != tls.VersionTLS12 ||
		dialer.HandshakeTimeout != 3*time.Second {
		t.Fatalf("NewWebSocketDialer() = %+v", dialer)
	}

	defaulted := NewWebSocketDialer(0)
	if defaulted.HandshakeTimeout != defaultWebSocketTimeout {
		t.Fatalf("default HandshakeTimeout = %v, want %v",
			defaulted.HandshakeTimeout, defaultWebSocketTimeout)
	}
}

func TestStatusErrorRedactsAndLimitsBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Body:       ioNopCloser{strings.NewReader(`{"accessToken":"secret","message":"no"}`)},
	}
	err := StatusError(errProtectBoom, resp, 1024)
	if err == nil {
		t.Fatal("StatusError() error = nil")
	}
	text := err.Error()
	if strings.Contains(text, "secret") || !strings.Contains(text, "<redacted>") {
		t.Fatalf("StatusError() = %q, want redacted token", text)
	}
}

func TestRedactSensitiveBearer(t *testing.T) {
	got := RedactSensitive("Authorization: Bearer abc.def")
	if strings.Contains(got, "abc.def") || !strings.Contains(got, "Bearer <redacted>") {
		t.Fatalf("RedactSensitive() = %q", got)
	}
}

func TestRetriableErrorIncludesEOF(t *testing.T) {
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF} {
		if !isRetriableError(err) {
			t.Fatalf("isRetriableError(%v) = false, want true", err)
		}
	}
}

type ioNopCloser struct {
	*strings.Reader
}

func (c ioNopCloser) Close() error { return nil }

func TestDialContextAndProxyDialer(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan struct{}, 2)
	go func() {
		for range 2 {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
			accepted <- struct{}{}
		}
	}()

	conn, err := DialContext(context.Background(), "tcp4", ln.Addr().String())
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	_ = conn.Close()

	proxyConn, err := NewProxyDialer().Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatalf("ProxyDialer.Dial() error = %v", err)
	}
	_ = proxyConn.Close()

	<-accepted
	<-accepted
}

func TestDialNetworksPreferIPv4ForGenericTCP(t *testing.T) {
	got := dialNetworks("tcp")
	want := []string{"tcp4", "tcp"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dialNetworks(tcp) = %v, want %v", got, want)
	}
	if got := dialNetworks("udp"); len(got) != 1 || got[0] != "udp" {
		t.Fatalf("dialNetworks(udp) = %v, want [udp]", got)
	}
}

func TestDialFailuresAreWrapped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, err := DialContext(ctx, "tcp4", "127.0.0.1:1"); err == nil {
		t.Fatal("DialContext() unexpectedly succeeded")
	}
	if _, err := NewProxyDialer().Dial("tcp4", "127.0.0.1:1"); err == nil {
		t.Fatal("ProxyDialer.Dial() unexpectedly succeeded")
	}
}

var _ syscall.RawConn = rawConnStub{}
