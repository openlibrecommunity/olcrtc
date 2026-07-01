package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/control"
	cryptopkg "github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/xtaci/smux"
)

var errUnexpectedConnectRequest = errors.New("unexpected connect request")

const (
	testConnectCommand = "connect"
	testConnectHost    = "example.com"
)

func TestSetupCipher(t *testing.T) {
	keyHex := "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	cipher, err := setupCipher(keyHex)
	if err != nil {
		t.Fatalf("setupCipher() error = %v", err)
	}
	if cipher == nil {
		t.Fatal("setupCipher() returned nil cipher")
	}
}

func TestSetupCipherRejectsBadInput(t *testing.T) {
	if _, err := setupCipher("zz"); err == nil {
		t.Fatal("setupCipher() unexpectedly succeeded for bad hex")
	}
	if _, err := setupCipher("00"); !errors.Is(err, ErrKeySize) {
		t.Fatalf("setupCipher() error = %v, want ErrKeySize", err)
	}
}

func TestSmuxConfig(t *testing.T) {
	cfg := smuxConfig(0)
	if cfg.Version != 2 || cfg.KeepAliveDisabled || cfg.MaxFrameSize != 32768 ||
		cfg.MaxReceiveBuffer != 8*1024*1024 || cfg.MaxStreamBuffer != 512*1024 {
		t.Fatalf("smuxConfig(0) = %+v", cfg)
	}
	capped := smuxConfig(4096)
	want := 4096 - runtime.SmuxWireOverhead
	if capped.MaxFrameSize != want {
		t.Fatalf("smuxConfig(4096).MaxFrameSize = %d, want %d",
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

func TestSOCKSSlotActiveCount(t *testing.T) {
	ctx := context.Background()
	c := &Client{
		socksSlots:          make(chan struct{}, 1),
		socksLimit:          1,
		socksTargets:        make(map[string]int64),
		socksPerTargetLimit: 1,
	}
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	if !c.acquireSOCKSSlot(ctx, server, "example.com:443") {
		t.Fatal("acquireSOCKSSlot() = false")
	}
	if got := c.socksActive.Load(); got != 1 {
		t.Fatalf("socksActive after acquire = %d, want 1", got)
	}

	c.releaseSOCKSSlot("example.com:443")
	if got := c.socksActive.Load(); got != 0 {
		t.Fatalf("socksActive after release = %d, want 0", got)
	}

	c.releaseSOCKSSlot("example.com:443")
	if got := c.socksActive.Load(); got != 0 {
		t.Fatalf("socksActive after extra release = %d, want 0", got)
	}
}

func TestSOCKSSlotRejectsWhenPerTargetLimitReached(t *testing.T) {
	ctx := context.Background()
	c := &Client{
		socksSlots:          make(chan struct{}, 64),
		socksLimit:          64,
		socksTargets:        make(map[string]int64),
		socksPerTargetLimit: 1,
	}
	firstServer, firstClient := net.Pipe()
	defer func() { _ = firstServer.Close() }()
	defer func() { _ = firstClient.Close() }()
	secondServer, secondClient := net.Pipe()
	defer func() { _ = secondServer.Close() }()
	defer func() { _ = secondClient.Close() }()

	if !c.acquireSOCKSSlot(ctx, firstServer, "mail.example:993") {
		t.Fatal("first acquireSOCKSSlot() = false")
	}
	if c.acquireSOCKSSlot(ctx, secondServer, "mail.example:993") {
		t.Fatal("second acquireSOCKSSlot() unexpectedly succeeded for same target")
	}
	if !c.acquireSOCKSSlot(ctx, secondServer, "www.example:443") {
		t.Fatal("acquireSOCKSSlot() for different target = false")
	}
}

func TestSOCKSSlotWaitsWhenGlobalLimitReached(t *testing.T) {
	c := &Client{
		socksSlots:          make(chan struct{}, 1),
		socksLimit:          1,
		socksTargets:        make(map[string]int64),
		socksPerTargetLimit: 8,
	}
	firstServer, firstClient := net.Pipe()
	defer func() { _ = firstServer.Close() }()
	defer func() { _ = firstClient.Close() }()
	secondServer, secondClient := net.Pipe()
	defer func() { _ = secondServer.Close() }()
	defer func() { _ = secondClient.Close() }()

	if !c.acquireSOCKSSlot(context.Background(), firstServer, "one.example:443") {
		t.Fatal("first acquireSOCKSSlot() = false")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if c.acquireSOCKSSlot(ctx, secondServer, "two.example:443") {
		t.Fatal("second acquireSOCKSSlot() unexpectedly succeeded while global slot was occupied")
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
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
	if !bytes.Equal(replySuccess(), []byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("replySuccess() = %v", replySuccess())
	}
	if !bytes.Equal(replyHostUnreachable(), []byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("replyHostUnreachable() = %v", replyHostUnreachable())
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

	serverSess, err := smux.Server(a, smuxConfig(0))
	if err != nil {
		t.Fatalf("smux.Server() error = %v", err)
	}
	defer func() { _ = serverSess.Close() }()
	clientSess, err := smux.Client(b, smuxConfig(0))
	if err != nil {
		t.Fatalf("smux.Client() error = %v", err)
	}
	defer func() { _ = clientSess.Close() }()

	done := make(chan error, 1)
	go func() {
		stream, err := serverSess.AcceptStream()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = stream.Close() }()

		var req map[string]any
		if err := json.NewDecoder(stream).Decode(&req); err != nil {
			done <- err
			return
		}
		if req["cmd"] != testConnectCommand || req["addr"] != testConnectHost {
			done <- errUnexpectedConnectRequest
			return
		}
		_, err = stream.Write([]byte{0x00})
		done <- err
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
	serverSess, err := smux.Server(a, smuxConfig(0))
	if err != nil {
		t.Fatalf("smux.Server() error = %v", err)
	}
	defer func() { _ = serverSess.Close() }()
	clientSess, err := smux.Client(b, smuxConfig(0))
	if err != nil {
		t.Fatalf("smux.Client() error = %v", err)
	}
	defer func() { _ = clientSess.Close() }()

	go func() {
		stream, err := serverSess.AcceptStream()
		if err != nil {
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

	serverSess, err := smux.Server(a, smuxConfig(0))
	if err != nil {
		t.Fatalf("smux.Server() error = %v", err)
	}
	defer func() { _ = serverSess.Close() }()
	clientSess, err := smux.Client(b, smuxConfig(0))
	if err != nil {
		t.Fatalf("smux.Client() error = %v", err)
	}
	defer func() { _ = clientSess.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := openControlStreamTimeout(ctx, clientSess, "dev", nil, time.Hour)
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

type closerLinkStub struct {
	closed     bool
	resetCount int
}

func (s *closerLinkStub) Connect(context.Context) error   { return nil }
func (s *closerLinkStub) Send([]byte) error               { return nil }
func (s *closerLinkStub) Close() error                    { s.closed = true; return nil }
func (s *closerLinkStub) SetReconnectCallback(func())     {}
func (s *closerLinkStub) SetShouldReconnect(func() bool)  {}
func (s *closerLinkStub) SetEndedCallback(func(string))   {}
func (s *closerLinkStub) WatchConnection(context.Context) {}
func (s *closerLinkStub) CanSend() bool                   { return true }
func (s *closerLinkStub) Features() transport.Features    { return transport.Features{} }
func (s *closerLinkStub) Reconnect(string)                {}
func (s *closerLinkStub) ResetPeer()                      { s.resetCount++ }

func TestOnDataWithNilConn(_ *testing.T) {
	c := &Client{}
	c.onData([]byte("ignored"))
}

func TestShutdownClosesLinkAndConn(t *testing.T) {
	cipher, err := cryptopkg.NewCipher("01234567890123456789012345678901")
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	ln := &closerLinkStub{}
	c := &Client{
		ln:     ln,
		cipher: cipher,
		conn:   muxconn.New(ln, cipher),
	}
	c.shutdown()
	if !ln.closed {
		t.Fatal("shutdown() did not close link")
	}
}

func TestResetLinkPeer(t *testing.T) {
	ln := &closerLinkStub{}
	c := &Client{ln: ln}
	c.resetLinkPeer()
	if ln.resetCount != 1 {
		t.Fatalf("ResetPeer calls = %d, want 1", ln.resetCount)
	}
}

//nolint:cyclop // integration-style control loop test needs setup and async assertions together
func TestStartControlLoopReportsPong(t *testing.T) {
	a, b := net.Pipe()
	defer func() {
		_ = a.Close()
		_ = b.Close()
	}()

	serverSess, err := smux.Server(a, smuxConfig(0))
	if err != nil {
		t.Fatalf("smux.Server() error = %v", err)
	}
	defer func() { _ = serverSess.Close() }()
	clientSess, err := smux.Client(b, smuxConfig(0))
	if err != nil {
		t.Fatalf("smux.Client() error = %v", err)
	}
	defer func() { _ = clientSess.Close() }()

	peerStreamCh := make(chan *smux.Stream, 1)
	go func() {
		stream, err := serverSess.AcceptStream()
		if err == nil {
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
	c.recordSession("sid-control")
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

func TestStatusRecordsReconnectAndUnhealthy(t *testing.T) {
	updates := 0
	c := &Client{health: runtime.NewHealthTracker(func(control.Status) { updates++ })}
	c.recordSession("sid-1")
	c.recordMissed(2)
	c.recordUnhealthy(3)
	c.recordReconnect()

	status := c.Status()
	if status.SessionID != "sid-1" || status.MissedPongs != 3 ||
		status.UnhealthyEvents != 1 || status.Reconnects != 1 || status.LastUnhealthy.IsZero() {
		t.Fatalf("Status() = %+v", status)
	}
	if updates != 4 {
		t.Fatalf("health updates = %d, want 4", updates)
	}
}
