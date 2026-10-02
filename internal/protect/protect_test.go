package protect

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

var errProtectBoom = errors.New("boom")

type rawConnStub struct {
	controlFn func(func(uintptr)) error
}

func restoreProtector(t *testing.T) {
	t.Helper()
	old := protector.Load()
	t.Cleanup(func() { protector.Store(old) })
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
	restoreProtector(t)
	SetProtector(nil)

	if err := controlFunc("tcp4", "", rawConnStub{}); err != nil {
		t.Fatalf("controlFunc() error = %v", err)
	}
}

func TestControlFuncWithProtector(t *testing.T) {
	restoreProtector(t)

	called := 0
	SetProtector(func(fd int) bool {
		called++
		if fd != 42 {
			t.Fatalf("protector fd = %d, want 42", fd)
		}
		return true
	})
	if err := controlFunc("tcp4", "", rawConnStub{}); err != nil {
		t.Fatalf("controlFunc() error = %v", err)
	}
	if called != 1 {
		t.Fatalf("protector calls = %d, want 1", called)
	}

	SetProtector(func(int) bool { return false })
	err := controlFunc("tcp4", "", rawConnStub{})
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "protect" {
		t.Fatalf("controlFunc() error = %v, want protect op error", err)
	}
}

func TestControlFuncWrapsControlError(t *testing.T) {
	restoreProtector(t)
	SetProtector(func(int) bool { return true })

	err := controlFunc("tcp4", "", rawConnStub{
		controlFn: func(func(uintptr)) error { return errProtectBoom },
	})
	if err == nil || err.Error() != "control failed: boom" {
		t.Fatalf("controlFunc() error = %v", err)
	}
}

func TestControlFuncSnapshotsProtector(t *testing.T) {
	restoreProtector(t)
	var firstCalls atomic.Int64
	var secondCalls atomic.Int64
	SetProtector(func(int) bool {
		firstCalls.Add(1)
		return true
	})
	err := controlFunc("tcp4", "", rawConnStub{controlFn: func(call func(uintptr)) error {
		SetProtector(func(int) bool {
			secondCalls.Add(1)
			return true
		})
		call(42)
		return nil
	}})
	if err != nil {
		t.Fatalf("controlFunc() error = %v", err)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("protector calls = %d/%d, want 1/0", firstCalls.Load(), secondCalls.Load())
	}
}

func TestConcurrentSetClearAndDialControl(t *testing.T) {
	restoreProtector(t)
	var calls atomic.Int64
	protectFunc := func(int) bool {
		calls.Add(1)
		return true
	}
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := range 500 {
				if (worker+iteration)%3 == 0 {
					SetProtector(nil)
				} else {
					SetProtector(protectFunc)
				}
				if err := controlFunc("tcp4", "", rawConnStub{}); err != nil {
					t.Errorf("controlFunc() error = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestNewDialerAndHTTPClient(t *testing.T) {
	dialer := NewDialer().netDialer()
	if dialer.Timeout != 10*time.Second || dialer.KeepAlive != 30*time.Second || dialer.Control == nil {
		t.Fatalf("netDialer() = %+v", dialer)
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

func TestLookupInjection(t *testing.T) {
	resolver := &net.Resolver{PreferGo: true}

	if got := NewDialer(resolver).lookup; got != resolver {
		t.Fatalf("NewDialer().lookup = %v, want %p", got, resolver)
	}
	if got := NewDialer(nil).lookup; got != nil {
		t.Fatalf("NewDialer(nil).lookup = %v, want nil", got)
	}
	if got := NewDialer(nil, resolver).lookup; got != resolver {
		t.Fatalf("NewDialer(nil, r).lookup = %v, want the first non-nil lookup", got)
	}
	if got := NewProxyDialer(resolver).dialer.lookup; got != resolver {
		t.Fatalf("NewProxyDialer().dialer.lookup = %v, want %p", got, resolver)
	}
}

func TestRetryTransportReplaysRequestBody(t *testing.T) {
	const payload = "<body rid=\"42\">test</body>"
	var bodies []string
	transport := &retryTransport{base: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		_ = req.Body.Close()
		bodies = append(bodies, string(body))
		if len(bodies) == 1 {
			return nil, &net.DNSError{Name: "bosh.test", Err: "temporary"}
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, "https://bosh.test/http-bind", bytes.NewBufferString(payload),
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	_ = resp.Body.Close()
	if len(bodies) != 2 || bodies[0] != payload || bodies[1] != payload {
		t.Fatalf("request bodies = %q, want two copies of %q", bodies, payload)
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
	got := redactSensitive("Authorization: Bearer abc.def")
	if strings.Contains(got, "abc.def") || !strings.Contains(got, "Bearer <redacted>") {
		t.Fatalf("redactSensitive() = %q", got)
	}
}

type ioNopCloser struct {
	*strings.Reader
}

func (c ioNopCloser) Close() error { return nil }

func TestProxyDialerDials(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan struct{}, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		_ = conn.Close()
		accepted <- struct{}{}
	}()

	proxyConn, err := NewProxyDialer().Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatalf("ProxyDialer.Dial() error = %v", err)
	}
	_ = proxyConn.Close()

	<-accepted
}

func TestDialFailuresAreWrapped(t *testing.T) {
	if _, err := NewProxyDialer().Dial("tcp4", "127.0.0.1:1"); err == nil {
		t.Fatal("ProxyDialer.Dial() unexpectedly succeeded")
	}
}

var _ syscall.RawConn = rawConnStub{}
