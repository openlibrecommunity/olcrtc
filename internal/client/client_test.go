package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/smux"

	"github.com/openlibrecommunity/olcrtc/internal/control"
	cryptopkg "github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/openlibrecommunity/olcrtc/internal/tunnelcore"
)

var errUnexpectedConnectRequest = errors.New("unexpected connect request")

const (
	testConnectCommand = "connect"
	testConnectHost    = "example.com"
)

func TestSetupKeySet(t *testing.T) {
	keyHex := "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	keys, err := tunnelcore.SetupKeySet(keyHex, cryptopkg.Client)
	if err != nil {
		t.Fatalf("SetupKeySet() error = %v", err)
	}
	if keys == nil {
		t.Fatal("SetupKeySet() returned nil key set")
	}
}

func TestSetupKeySetRejectsBadInput(t *testing.T) {
	if _, err := tunnelcore.SetupKeySet("zz", cryptopkg.Client); err == nil {
		t.Fatal("SetupKeySet() unexpectedly succeeded for bad hex")
	}
	if _, err := tunnelcore.SetupKeySet("00", cryptopkg.Client); !errors.Is(err, ErrKeySize) {
		t.Fatalf("SetupKeySet() error = %v, want ErrKeySize", err)
	}
}

func newClientTestKeys(t *testing.T) *cryptopkg.KeySet {
	t.Helper()
	keys, err := cryptopkg.NewKeySet([]byte("01234567890123456789012345678901"), cryptopkg.Client)
	if err != nil {
		t.Fatalf("NewKeySet(client) error = %v", err)
	}
	return keys
}

// testSmuxCfg is the data-plane smux config buildSmuxClient builds for a plain
// (non control-plane) transport.
func testSmuxCfg() *smux.Config {
	return runtime.SmuxConfigFor(&closerLinkStub{})
}

func TestDataSmuxConfig(t *testing.T) {
	cfg := runtime.SmuxConfigFor(&closerLinkStub{})
	if cfg.Version != 2 || cfg.KeepAliveDisabled || cfg.MaxFrameSize != 32768 ||
		cfg.MaxReceiveBuffer != 32*1024*1024 || cfg.MaxStreamBuffer != 4*1024*1024 {
		t.Fatalf("SmuxConfigFor(plain link) = %+v", cfg)
	}
	capped := runtime.SmuxConfigFor(&closerLinkStub{maxPayload: 4096})
	want := 4096 - runtime.SmuxWireOverhead
	if capped.MaxFrameSize != want {
		t.Fatalf("SmuxConfigFor(maxPayload=4096).MaxFrameSize = %d, want %d",
			capped.MaxFrameSize, want)
	}
}

func TestSocks5Handshake(t *testing.T) {
	c := &Client{}
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()

	done := make(chan error, 1)
	go func() {
		done <- c.socks5Handshake(server)
	}()

	if _, err := client.Write([]byte{5, 1, 0}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(client, resp); err != nil {
		t.Fatalf("ReadFull() error = %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("socks5Handshake() error = %v", err)
	}
	if !bytes.Equal(resp, []byte{5, 0}) {
		t.Fatalf("handshake response = %v, want [5 0]", resp)
	}
}

func TestSocks5HandshakeWithAuth(t *testing.T) {
	c := &Client{socksUser: "user", socksPass: "pass"}
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()

	done := make(chan error, 1)
	go func() {
		done <- c.socks5Handshake(server)
	}()

	// Client greeting: VER=5, NMETHODS=1, METHOD=0x02 (user/pass)
	if _, err := client.Write([]byte{5, 1, 2}); err != nil {
		t.Fatalf("Write greeting: %v", err)
	}
	// Server must reply with method 0x02 (username/password)
	resp := make([]byte, 2)
	if _, err := io.ReadFull(client, resp); err != nil {
		t.Fatalf("ReadFull method: %v", err)
	}
	if !bytes.Equal(resp, []byte{5, 2}) {
		t.Fatalf("method selection = %v, want [5 2]", resp)
	}
	// Send the auth sub-negotiation: VER(1) + ULEN(1) + USER + PLEN(1) + PASS
	authReq := make([]byte, 0, 11)
	authReq = append(authReq, 0x01, 0x04)
	authReq = append(authReq, []byte("user")...)
	authReq = append(authReq, 0x04)
	authReq = append(authReq, []byte("pass")...)
	if _, err := client.Write(authReq); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	// Read the auth response
	authResp := make([]byte, 2)
	if _, err := io.ReadFull(client, authResp); err != nil {
		t.Fatalf("read auth response: %v", err)
	}
	if !bytes.Equal(authResp, []byte{0x01, 0x00}) {
		t.Fatalf("auth response = %v, want [1 0]", authResp)
	}

	if err := <-done; err != nil {
		t.Fatalf("socks5Handshake() error = %v", err)
	}
}

func TestSocks5HandshakeAuthRejected(t *testing.T) {
	c := &Client{socksUser: "user", socksPass: "right"}
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()

	done := make(chan error, 1)
	go func() {
		done <- c.socks5Handshake(server)
	}()

	if _, err := client.Write([]byte{5, 1, 2}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	// Consume method selection reply [5, 2]
	resp := make([]byte, 2)
	if _, err := io.ReadFull(client, resp); err != nil {
		t.Fatalf("ReadFull method: %v", err)
	}
	// Send wrong credentials
	authReq := make([]byte, 0, 12)
	authReq = append(authReq, 0x01, 0x04)
	authReq = append(authReq, []byte("user")...)
	authReq = append(authReq, 0x05)
	authReq = append(authReq, []byte("wrong")...)
	if _, err := client.Write(authReq); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	// Server should reply with failure [0x01, 0x01]
	authResp := make([]byte, 2)
	if _, err := io.ReadFull(client, authResp); err != nil {
		t.Fatalf("read auth response: %v", err)
	}
	if !bytes.Equal(authResp, []byte{0x01, 0x01}) {
		t.Fatalf("auth response = %v, want [1 1]", authResp)
	}

	if err := <-done; !errors.Is(err, ErrSOCKSAuthFailed) {
		t.Fatalf("socks5Handshake() error = %v, want ErrSOCKSAuthFailed", err)
	}
}

func TestSocks5HandshakeRejectsVersion(t *testing.T) {
	c := &Client{}
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()

	done := make(chan error, 1)
	go func() {
		done <- c.socks5Handshake(server)
	}()

	if _, err := client.Write([]byte{4, 1}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if err := <-done; !errors.Is(err, ErrInvalidSOCKSVersion) {
		t.Fatalf("socks5Handshake() error = %v, want %v", err, ErrInvalidSOCKSVersion)
	}
}

func TestSocks5HandshakeReadMethodsError(t *testing.T) {
	c := &Client{}
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()

	done := make(chan error, 1)
	go func() {
		done <- c.socks5Handshake(server)
	}()

	if _, err := client.Write([]byte{5, 2, 0}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	_ = client.Close()
	if err := <-done; err == nil {
		t.Fatal("socks5Handshake() unexpectedly succeeded")
	}
}

func TestSocks5RequestIPv4(t *testing.T) {
	c := &Client{}
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()

	done := make(chan struct {
		addr string
		port int
		err  error
	}, 1)
	go func() {
		addr, port, err := c.socks5Request(server)
		done <- struct {
			addr string
			port int
			err  error
		}{addr: addr, port: port, err: err}
	}()

	req := []byte{5, 1, 0, 1, 127, 0, 0, 1}
	port := make([]byte, 2)
	binary.BigEndian.PutUint16(port, 8080)
	if _, err := client.Write(append(req, port...)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	res := <-done
	if res.err != nil {
		t.Fatalf("socks5Request() error = %v", res.err)
	}
	if res.addr != "127.0.0.1" || res.port != 8080 {
		t.Fatalf("socks5Request() = (%q, %d), want (127.0.0.1, 8080)", res.addr, res.port)
	}
}

func TestSocks5RequestDomain(t *testing.T) {
	c := &Client{}
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()

	done := make(chan struct {
		addr string
		port int
		err  error
	}, 1)
	go func() {
		addr, port, err := c.socks5Request(server)
		done <- struct {
			addr string
			port int
			err  error
		}{addr: addr, port: port, err: err}
	}()

	req := make([]byte, 0, 16)
	req = append(req, 5, 1, 0, 3, 11)
	req = append(req, []byte("example.com")...)
	port := make([]byte, 2)
	binary.BigEndian.PutUint16(port, 443)
	if _, err := client.Write(append(req, port...)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	res := <-done
	if res.err != nil {
		t.Fatalf("socks5Request() error = %v", res.err)
	}
	if res.addr != "example.com" || res.port != 443 {
		t.Fatalf("socks5Request() = (%q, %d), want (example.com, 443)", res.addr, res.port)
	}
}

func TestSocks5RequestRejectsCommandAndAddressType(t *testing.T) {
	c := &Client{}
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()

	done := make(chan error, 1)
	go func() {
		_, _, err := c.socks5Request(server)
		done <- err
	}()

	if _, err := client.Write([]byte{5, 2, 0, 1}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	// An unsupported command is refused with a reply, not by hanging up, so the
	// caller has to drain it: net.Pipe is unbuffered and the write would block.
	reply := make([]byte, 10)
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatalf("ReadFull() error = %v", err)
	}
	if reply[0] != socksVersion || reply[1] != socksRepCommandNotSupported {
		t.Fatalf("reply = %v, want version %d rep %d", reply, socksVersion, socksRepCommandNotSupported)
	}

	if err := <-done; !errors.Is(err, ErrUnsupportedSOCKSCommand) {
		t.Fatalf("socks5Request() error = %v, want %v", err, ErrUnsupportedSOCKSCommand)
	}

	server2, client2 := net.Pipe()
	defer func() {
		_ = server2.Close()
		_ = client2.Close()
	}()

	done = make(chan error, 1)
	go func() {
		_, _, err := c.socks5Request(server2)
		done <- err
	}()

	if _, err := client2.Write([]byte{5, 1, 0, 9}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if err := <-done; !errors.Is(err, ErrUnsupportedAddressType) {
		t.Fatalf("socks5Request() error = %v, want %v", err, ErrUnsupportedAddressType)
	}
}

func TestSocks5RequestReadPortError(t *testing.T) {
	c := &Client{}
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()

	done := make(chan error, 1)
	go func() {
		_, _, err := c.socks5Request(server)
		done <- err
	}()

	if _, err := client.Write([]byte{5, 1, 0, 1, 127, 0, 0, 1, 0}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	_ = client.Close()
	if err := <-done; err == nil {
		t.Fatal("socks5Request() unexpectedly succeeded")
	}
}

func TestReplyBuffers(t *testing.T) {
	if !bytes.Equal(replySuccess(testConnectHost), []byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("replySuccess() = %v", replySuccess(testConnectHost))
	}
	if !bytes.Equal(replyHostUnreachable(testConnectHost), []byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("replyHostUnreachable() = %v", replyHostUnreachable(testConnectHost))
	}
	// An IPv6 target must be answered with an IPv6 bound address.
	want := append([]byte{5, 0, 0, 4}, make([]byte, 18)...)
	if got := replySuccess("2001:db8::1"); !bytes.Equal(got, want) {
		t.Fatalf("replySuccess(ipv6) = %v, want %v", got, want)
	}
	want[1] = 4
	if got := replyHostUnreachable("2001:db8::1"); !bytes.Equal(got, want) {
		t.Fatalf("replyHostUnreachable(ipv6) = %v, want %v", got, want)
	}
	// An IPv4 literal keeps the IPv4 form.
	if got := replySuccess("127.0.0.1"); len(got) != 10 || got[3] != socksAddrIPv4 {
		t.Fatalf("replySuccess(ipv4) = %v", got)
	}
}

func TestReadSocks5AddrReadErrors(t *testing.T) {
	c := &Client{}
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := c.readSocks5Addr(server, 1)
		done <- err
	}()

	time.Sleep(10 * time.Millisecond)
	_ = client.Close()
	if err := <-done; err == nil {
		t.Fatal("readSocks5Addr() unexpectedly succeeded")
	}
}

func TestSendConnectRequestOverSmux(t *testing.T) {
	a, b := net.Pipe()
	defer func() {
		_ = a.Close()
		_ = b.Close()
	}()

	serverSess, err := smux.Server(a, testSmuxCfg())
	if err != nil {
		t.Fatalf("smux.Server() error = %v", err)
	}
	defer func() { _ = serverSess.Close() }()
	clientSess, err := smux.Client(b, testSmuxCfg())
	if err != nil {
		t.Fatalf("smux.Client() error = %v", err)
	}
	defer func() { _ = clientSess.Close() }()

	done := make(chan error, 1)
	go func() {
		stream, acceptErr := serverSess.AcceptStream()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		defer func() { _ = stream.Close() }()

		var req map[string]any
		if decodeErr := json.NewDecoder(stream).Decode(&req); decodeErr != nil {
			done <- decodeErr
			return
		}
		if req["cmd"] != testConnectCommand || req["addr"] != testConnectHost {
			done <- errUnexpectedConnectRequest
			return
		}
		_, writeErr := stream.Write([]byte{0x00})
		done <- writeErr
	}()

	stream, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	defer func() { _ = stream.Close() }()

	c := &Client{deviceID: "client-1"}
	if err := c.sendConnectRequest(stream, testConnectHost, 443); err != nil {
		t.Fatalf("sendConnectRequest() error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("server side error = %v", err)
	}
}

func TestSendConnectRequestRejectsBadAck(t *testing.T) {
	a, b := net.Pipe()
	defer func() {
		_ = a.Close()
		_ = b.Close()
	}()
	serverSess, err := smux.Server(a, testSmuxCfg())
	if err != nil {
		t.Fatalf("smux.Server() error = %v", err)
	}
	defer func() { _ = serverSess.Close() }()
	clientSess, err := smux.Client(b, testSmuxCfg())
	if err != nil {
		t.Fatalf("smux.Client() error = %v", err)
	}
	defer func() { _ = clientSess.Close() }()

	go func() {
		stream, acceptErr := serverSess.AcceptStream()
		if acceptErr != nil {
			return
		}
		defer func() { _ = stream.Close() }()
		_, _ = io.CopyN(io.Discard, stream, 1)
		_, _ = stream.Write([]byte{0x01})
	}()

	stream, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	defer func() { _ = stream.Close() }()

	c := &Client{deviceID: "client-1"}
	if err := c.sendConnectRequest(stream, "example.com", 443); !errors.Is(err, ErrRemoteNotReady) {
		t.Fatalf("sendConnectRequest() error = %v, want %v", err, ErrRemoteNotReady)
	}
}

func TestOpenControlStreamStopsOnContextCancel(t *testing.T) {
	a, b := net.Pipe()
	defer func() {
		_ = a.Close()
		_ = b.Close()
	}()

	serverSess, err := smux.Server(a, testSmuxCfg())
	if err != nil {
		t.Fatalf("smux.Server() error = %v", err)
	}
	defer func() { _ = serverSess.Close() }()
	clientSess, err := smux.Client(b, testSmuxCfg())
	if err != nil {
		t.Fatalf("smux.Client() error = %v", err)
	}
	defer func() { _ = clientSess.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, _, err := openControlStreamTimeout(ctx, clientSess, "dev", nil, time.Hour)
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("openControlStreamTimeout() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for context cancellation")
	}
}

// methods below are new (peer-restart-corroboration PR); closed/resetCount
// and the rest of the stub predate it.
type closerLinkStub struct {
	closed     bool
	resetCount int
	maxPayload int

	mu        sync.Mutex
	unhealthy []bool
	sends     int
	sentCh    chan struct{}
}

func (s *closerLinkStub) Connect(context.Context) error { return nil }
func (s *closerLinkStub) Send([]byte) error {
	s.mu.Lock()
	s.sends++
	ch := s.sentCh
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return nil
}
func (s *closerLinkStub) Close() error                    { s.closed = true; return nil }
func (s *closerLinkStub) SetReconnectCallback(func())     {}
func (s *closerLinkStub) SetShouldReconnect(func() bool)  {}
func (s *closerLinkStub) SetEndedCallback(func(string))   {}
func (s *closerLinkStub) WatchConnection(context.Context) {}
func (s *closerLinkStub) CanSend() bool                   { return true }
func (s *closerLinkStub) Features() transport.Features {
	return transport.Features{MaxPayloadSize: s.maxPayload}
}
func (s *closerLinkStub) Reconnect(string) {}
func (s *closerLinkStub) ResetPeer()       { s.resetCount++ }

func (s *closerLinkStub) NotifyLinkHealth(unhealthy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unhealthy = append(s.unhealthy, unhealthy)
}

// lastNotified returns the most recently pushed NotifyLinkHealth value and
// whether any call has happened yet.
func (s *closerLinkStub) lastNotified() (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.unhealthy) == 0 {
		return false, false
	}
	return s.unhealthy[len(s.unhealthy)-1], true
}

func TestOnDataWithNilConn(_ *testing.T) {
	c := &Client{}
	c.onData([]byte("ignored"))
}

func TestShutdownClosesLinkAndConn(t *testing.T) {
	keys := newClientTestKeys(t)
	ln := &closerLinkStub{}
	c := &Client{
		ln:   ln,
		keys: keys,
		conn: muxconn.New(ln, keys),
	}
	c.shutdown()
	if !ln.closed {
		t.Fatal("shutdown() did not close link")
	}
}

func TestResetLinkPeer(t *testing.T) {
	ln := &closerLinkStub{}
	c := &Client{ln: ln}
	tunnelcore.ResetPeer(c.ln)
	if ln.resetCount != 1 {
		t.Fatalf("ResetPeer calls = %d, want 1", ln.resetCount)
	}
}

func TestStartControlLoopReportsPong(t *testing.T) {
	a, b := net.Pipe()
	defer func() {
		_ = a.Close()
		_ = b.Close()
	}()

	serverSess, err := smux.Server(a, testSmuxCfg())
	if err != nil {
		t.Fatalf("smux.Server() error = %v", err)
	}
	defer func() { _ = serverSess.Close() }()
	clientSess, err := smux.Client(b, testSmuxCfg())
	if err != nil {
		t.Fatalf("smux.Client() error = %v", err)
	}
	defer func() { _ = clientSess.Close() }()

	peerStreamCh := make(chan *smux.Stream, 1)
	go func() {
		stream, acceptErr := serverSess.AcceptStream()
		if acceptErr == nil {
			peerStreamCh <- stream
		}
	}()

	stream, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	peerStream := <-peerStreamCh

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan control.Health, 1)
	c := &Client{sessionID: "sid-control", health: runtime.NewHealthTracker(nil)}
	c.health.RecordSession("sid-control")
	c.startControlLoop(ctx, Config{
		Liveness: control.Config{
			Interval: 10 * time.Millisecond,
			Timeout:  100 * time.Millisecond,
			Failures: 2,
			OnPong: func(h control.Health) {
				select {
				case got <- h:
				default:
				}
			},
		},
	}, cancel, stream)
	go func() {
		_ = control.Run(ctx, peerStream, control.Config{
			Interval: 10 * time.Millisecond,
			Timeout:  100 * time.Millisecond,
			Failures: 2,
		})
	}()

	select {
	case h := <-got:
		if h.Seq == 0 {
			t.Fatal("Health.Seq = 0")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for control pong")
	}
	status := c.Status()
	if status.SessionID != "sid-control" {
		t.Fatalf("Status.SessionID = %q, want sid-control", status.SessionID)
	}
	if status.LastPong.IsZero() || status.LastRTT < 0 || status.MissedPongs != 0 {
		t.Fatalf("Status() = %+v", status)
	}
}

// TestWatchControlStalenessNotifiesTransport unit-tests watchControlStaleness
// directly (not through the full control.Run/smux stack - control.Run always
// closes its stream when its context is done, so "stop responding but keep
// the stream open" can't be simulated that way). Confirms it pushes
// NotifyLinkHealth(false) while controlLastPong is fresh, and flips to
// true once the last pong ages past the 2x-interval staleness threshold - on
// a timescale close to the ping interval, not the relaxed
// OnMissedPong/OnUnhealthy thresholds (45-90s for vp8channel).
func TestWatchControlStalenessNotifiesTransport(t *testing.T) {
	const interval = 5 * time.Millisecond

	ln := &closerLinkStub{}
	c := &Client{ln: ln}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c.controlLastPong.Store(time.Now())
	go c.watchControlStaleness(ctx, interval)

	deadline := time.Now().Add(time.Second)
	for {
		if v, ok := ln.lastNotified(); ok && !v {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for NotifyLinkHealth(false)")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Let the last pong age past the staleness threshold without refreshing
	// it - simulates the peer going silent while the link itself stays up.
	deadline = time.Now().Add(time.Second)
	for {
		if v, ok := ln.lastNotified(); ok && v {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for NotifyLinkHealth(true)")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestStatusRecordsReconnectAndUnhealthy(t *testing.T) {
	updates := 0
	c := &Client{health: runtime.NewHealthTracker(func(control.Status) { updates++ })}
	c.health.RecordSession("sid-1")
	c.health.RecordMissed(2)
	c.health.RecordUnhealthy(3)
	c.health.RecordReconnect()

	status := c.Status()
	if status.SessionID != "sid-1" || status.MissedPongs != 3 ||
		status.UnhealthyEvents != 1 || status.Reconnects != 1 || status.LastUnhealthy.IsZero() {
		t.Fatalf("Status() = %+v", status)
	}
	if updates != 4 {
		t.Fatalf("health updates = %d, want 4", updates)
	}
}

func TestSocks5RequestIPv6(t *testing.T) {
	c := &Client{}
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()

	type result struct {
		addr string
		port int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		addr, port, err := c.socks5Request(server)
		done <- result{addr: addr, port: port, err: err}
	}()

	req := make([]byte, 0, 4+net.IPv6len+2)
	req = append(req, 5, 1, 0, socksAddrIPv6)
	req = append(req, net.ParseIP("2001:db8::1").To16()...)
	port := make([]byte, 2)
	binary.BigEndian.PutUint16(port, 8443)
	req = append(req, port...)
	if _, err := client.Write(req); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	res := <-done
	if res.err != nil {
		t.Fatalf("socks5Request() error = %v", res.err)
	}
	if res.addr != "2001:db8::1" || res.port != 8443 {
		t.Fatalf("socks5Request() = (%q, %d), want (2001:db8::1, 8443)", res.addr, res.port)
	}
}

func TestReadSocks5AddrIPv6ReadError(t *testing.T) {
	c := &Client{}
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := c.readSocks5Addr(server, socksAddrIPv6)
		done <- err
	}()

	time.Sleep(10 * time.Millisecond)
	_ = client.Close()
	if err := <-done; err == nil {
		t.Fatal("readSocks5Addr(ipv6) unexpectedly succeeded")
	}
}

// TestSendConnectRequestMapsNegativeAck covers the negative CONNECT ack the
// server now sends on dial failure: it must fail immediately instead of sitting
// on the ack deadline, and the code must reach the local application as the
// matching SOCKS5 reply. 0x05 is not emitted by today's server but pins the
// pass-through, so a future code is not silently flattened.
func TestSendConnectRequestMapsNegativeAck(t *testing.T) {
	for _, ack := range []byte{tunnelcore.ConnectAckHostUnreachable, 0x05} {
		elapsed, err := connectWithAck(t, ack)
		if !errors.Is(err, ErrRemoteNotReady) {
			t.Fatalf("ack=0x%02x: sendConnectRequest() error = %v, want %v", ack, err, ErrRemoteNotReady)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("ack=0x%02x: sendConnectRequest() blocked for %v", ack, elapsed)
		}
		reply := replyForConnectError(err, testConnectHost)
		if !bytes.Equal(reply, []byte{5, ack, 0, 1, 0, 0, 0, 0, 0, 0}) {
			t.Fatalf("ack=0x%02x: replyForConnectError() = %v", ack, reply)
		}
	}
}

// connectWithAck runs one tunnel CONNECT against a stub server that answers
// with the given ack byte, and reports how long the client took to surface the
// result plus the error it produced.
func connectWithAck(t *testing.T, ack byte) (time.Duration, error) {
	t.Helper()
	a, b := net.Pipe()
	defer func() {
		_ = a.Close()
		_ = b.Close()
	}()
	serverSess, err := smux.Server(a, testSmuxCfg())
	if err != nil {
		t.Fatalf("smux.Server() error = %v", err)
	}
	defer func() { _ = serverSess.Close() }()
	clientSess, err := smux.Client(b, testSmuxCfg())
	if err != nil {
		t.Fatalf("smux.Client() error = %v", err)
	}
	defer func() { _ = clientSess.Close() }()

	go func() {
		peer, acceptErr := serverSess.AcceptStream()
		if acceptErr != nil {
			return
		}
		defer func() { _ = peer.Close() }()
		_, _ = io.CopyN(io.Discard, peer, 1)
		_, _ = peer.Write([]byte{ack})
	}()

	stream, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	defer func() { _ = stream.Close() }()

	c := &Client{deviceID: "client-1"}
	start := time.Now()
	err = c.sendConnectRequest(stream, testConnectHost, 443)
	return time.Since(start), err
}

// TestReplyForConnectErrorFallsBackToHostUnreachable covers a failure that
// never produced an ack byte at all (write error, timeout).
func TestReplyForConnectErrorFallsBackToHostUnreachable(t *testing.T) {
	reply := replyForConnectError(ErrRemoteNotReady, testConnectHost)
	if !bytes.Equal(reply, replyHostUnreachable(testConnectHost)) {
		t.Fatalf("replyForConnectError() = %v", reply)
	}
}

// TestShutdownWaitsForTrackedGoroutines guards the goroutine tracking: the
// client used to start acceptLoop, WatchConnection, watchControlStaleness and
// the control loop completely untracked, so a returning Run left them running.
func TestShutdownWaitsForTrackedGoroutines(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{ln: &closerLinkStub{}}
	var finished atomic.Bool
	c.goTracked(func() {
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		finished.Store(true)
	})

	cancel()
	c.shutdown()
	if !finished.Load() {
		t.Fatal("shutdown() returned before the tracked goroutine finished")
	}
}

// TestShutdownGiveUpOnStuckGoroutine is the other half of the contract: the
// wait is bounded, so a wedged goroutine cannot hang the process on exit.
func TestShutdownGivesUpOnStuckGoroutine(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	c := &Client{ln: &closerLinkStub{}, shutdownGrace: 20 * time.Millisecond}
	c.goTracked(func() { <-release })

	done := make(chan struct{})
	go func() {
		c.shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown() hung on a stuck goroutine")
	}
}

// TestLivenessFallbackReestablishesSession covers the case where the provider
// never calls back after a liveness-triggered rebuild. handleReconnect returns
// straight after ln.Reconnect and relies on that callback; without a fallback,
// sessionReady is never signalled again and every SOCKS connection fails on the
// 60s readiness gate. The fallback proves it acted by driving a handshake over
// the link.
func TestLivenessFallbackReestablishesSession(t *testing.T) {
	keys := newClientTestKeys(t)
	ln := &closerLinkStub{sentCh: make(chan struct{}, 1)}
	c := &Client{
		ln:               ln,
		keys:             keys,
		deviceID:         "dev-1",
		health:           runtime.NewHealthTracker(nil),
		sessionReady:     make(chan struct{}),
		livenessFallback: 10 * time.Millisecond,
		shutdownGrace:    2 * time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Drive the real path: a liveness reconnect hands the rebuild to the
	// provider and arms the fallback.
	c.handleReconnect(ctx, Config{}, cancel, reconnectLiveness)

	select {
	case <-ln.sentCh:
	case <-time.After(3 * time.Second):
		t.Fatal("liveness fallback never tried to re-establish the session")
	}
	cancel()
	c.waitGoroutines()
}

// TestLivenessFallbackSkipsWhenSessionIsBack makes sure the fallback stays out
// of the way when the provider callback did its job.
func TestLivenessFallbackSkipsWhenSessionIsBack(t *testing.T) {
	a, b := net.Pipe()
	defer func() {
		_ = a.Close()
		_ = b.Close()
	}()
	sess, err := smux.Client(a, testSmuxCfg())
	if err != nil {
		t.Fatalf("smux.Client() error = %v", err)
	}
	defer func() { _ = sess.Close() }()

	ln := &closerLinkStub{}
	c := &Client{
		ln:               ln,
		health:           runtime.NewHealthTracker(nil),
		sessionReady:     make(chan struct{}),
		session:          sess,
		sessionID:        "sid-1",
		livenessFallback: 10 * time.Millisecond,
		shutdownGrace:    time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.scheduleLivenessFallback(ctx, Config{}, cancel)
	c.waitGoroutines()

	ln.mu.Lock()
	sends := ln.sends
	ln.mu.Unlock()
	if sends != 0 {
		t.Fatalf("fallback ran with a healthy session (link sends = %d)", sends)
	}
}

// TestClientLinkAccessIsRaceFree exercises the c.ln readers that used to
// disagree about locking (resetLinkPeer under sessMu.RLock, notifyLinkHealth
// and tryReopenSession unlocked) together with the session-state accessors.
// Run with -race.
func TestClientLinkAccessIsRaceFree(t *testing.T) {
	keys := newClientTestKeys(t)
	ln := &closerLinkStub{}
	c := &Client{ln: ln, keys: keys, sessionReady: make(chan struct{})}

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func() {
			defer wg.Done()
			switch i % 4 {
			case 0:
				c.notifyLinkHealth(true)
			case 1:
				c.onData([]byte("frame"))
			case 2:
				c.signalSessionReady()
			default:
				_ = c.readyChannel()
				_ = c.sessionEstablished()
			}
		}()
	}
	wg.Wait()
}

func TestNoteConnectFailureLatchesMissingIPv6(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		target string
		want   bool
	}{
		{
			name:   "ipv6 unreachable latches",
			err:    &connectAckError{code: socksRepHostUnreachable},
			target: "2606:4700:4700::1111",
			want:   true,
		},
		{
			name:   "ipv4 unreachable does not latch",
			err:    &connectAckError{code: socksRepHostUnreachable},
			target: "1.1.1.1",
			want:   false,
		},
		{
			name:   "domain does not latch",
			err:    &connectAckError{code: socksRepHostUnreachable},
			target: "example.com",
			want:   false,
		},
		{
			name:   "other ack code does not latch",
			err:    &connectAckError{code: 3},
			target: "2606:4700:4700::1111",
			want:   false,
		},
		{
			name:   "non-ack error does not latch",
			err:    ErrRemoteNotReady,
			target: "2606:4700:4700::1111",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{}
			c.noteConnectFailure(tt.err, tt.target)
			if got := c.peerNoIPv6.Load(); got != tt.want {
				t.Fatalf("peerNoIPv6 = %v, want %v", got, tt.want)
			}
		})
	}
}

// Once the exit is known to have no IPv6, the refusal must come from the client
// itself: the point of the latch is that no tunnel stream is spent on it. With
// no session installed, reaching the tunnel path would instead park the request
// on the session-ready wait, so an immediate reply is what proves the shortcut.
func TestHandleSocks5RefusesIPv6LocallyWhenExitHasNone(t *testing.T) {
	c := &Client{sessionReady: make(chan struct{})}
	c.peerNoIPv6.Store(true)
	server, client := net.Pipe()
	defer func() {
		_ = server.Close()
		_ = client.Close()
	}()

	go c.handleSocks5(context.Background(), server)

	if _, err := client.Write([]byte{5, 1, 0}); err != nil {
		t.Fatalf("Write() greeting error = %v", err)
	}
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(client, greeting); err != nil {
		t.Fatalf("ReadFull() greeting error = %v", err)
	}

	request := []byte{5, socksCmdConnect, 0, socksAddrIPv6}
	request = append(request, net.ParseIP("2606:4700:4700::1111").To16()...)
	request = append(request, 1, 187)
	if _, err := client.Write(request); err != nil {
		t.Fatalf("Write() request error = %v", err)
	}

	replyCh := make(chan []byte, 1)
	go func() {
		reply := make([]byte, 22)
		if _, err := io.ReadFull(client, reply); err != nil {
			close(replyCh)
			return
		}
		replyCh <- reply
	}()

	select {
	case reply, ok := <-replyCh:
		if !ok {
			t.Fatal("ReadFull() reply failed")
		}
		if reply[1] != socksRepHostUnreachable {
			t.Fatalf("reply rep = %d, want %d", reply[1], socksRepHostUnreachable)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("IPv6 request was not refused locally")
	}
}
