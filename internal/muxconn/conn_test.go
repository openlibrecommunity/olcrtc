package muxconn

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	cryptopkg "github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
)

var errMuxBoom = errors.New("boom")

type stubLink struct {
	mu        sync.Mutex
	canSend   bool
	sendErr   error
	sent      [][]byte
	peerSent  map[string][][]byte
	canSendFn func() bool
}

func (s *stubLink) Connect(context.Context) error   { return nil }
func (s *stubLink) Close() error                    { return nil }
func (s *stubLink) SetReconnectCallback(func())     {}
func (s *stubLink) SetShouldReconnect(func() bool)  {}
func (s *stubLink) SetEndedCallback(func(string))   {}
func (s *stubLink) WatchConnection(context.Context) {}
func (s *stubLink) Reconnect(string)                {}
func (s *stubLink) Features() transport.Features    { return transport.Features{} }
func (s *stubLink) Send(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, append([]byte(nil), data...))
	return s.sendErr
}
func (s *stubLink) CanSend() bool {
	if s.canSendFn != nil {
		return s.canSendFn()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.canSend
}
func (s *stubLink) SendTo(peerID string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peerSent == nil {
		s.peerSent = make(map[string][][]byte)
	}
	s.peerSent[peerID] = append(s.peerSent[peerID], append([]byte(nil), data...))
	return s.sendErr
}
func (s *stubLink) SupportsPeerRouting() bool { return true }

type loopLink struct {
	peer          *loopLink
	onData        func([]byte)
	controlOnData func([]byte)
}

func (l *loopLink) Connect(context.Context) error   { return nil }
func (l *loopLink) Close() error                    { return nil }
func (l *loopLink) SetReconnectCallback(func())     {}
func (l *loopLink) SetShouldReconnect(func() bool)  {}
func (l *loopLink) SetEndedCallback(func(string))   {}
func (l *loopLink) WatchConnection(context.Context) {}
func (l *loopLink) CanSend() bool                   { return true }
func (l *loopLink) Features() transport.Features    { return transport.Features{} }
func (l *loopLink) Reconnect(string)                {}
func (l *loopLink) Send(data []byte) error {
	if l.peer.onData != nil {
		l.peer.onData(bytes.Clone(data))
	}
	return nil
}
func (l *loopLink) ControlSend(data []byte) error {
	if l.peer.controlOnData != nil {
		l.peer.controlOnData(bytes.Clone(data))
	}
	return nil
}
func (l *loopLink) SetControlOnData(cb func([]byte)) { l.controlOnData = cb }
func (l *loopLink) ControlCanSend() bool             { return true }

func newTestKeyPair(t *testing.T) (*cryptopkg.KeySet, *cryptopkg.KeySet) {
	t.Helper()
	client, err := cryptopkg.NewKeySet([]byte("01234567890123456789012345678901"), cryptopkg.Client)
	if err != nil {
		t.Fatalf("NewKeySet(client) error = %v", err)
	}
	server, err := cryptopkg.NewKeySet([]byte("01234567890123456789012345678901"), cryptopkg.Server)
	if err != nil {
		t.Fatalf("NewKeySet(server) error = %v", err)
	}
	return client, server
}

func TestPushAndReadRoundTrip(t *testing.T) {
	clientKeys, serverKeys := newTestKeyPair(t)
	conn := New(&stubLink{canSend: true}, serverKeys)

	msg1, err := clientKeys.Seal([]byte("hello "), []byte(dataRecordAAD))
	if err != nil {
		t.Fatalf("Seal(msg1) error = %v", err)
	}
	msg2, err := clientKeys.Seal([]byte("world"), []byte(dataRecordAAD))
	if err != nil {
		t.Fatalf("Seal(msg2) error = %v", err)
	}

	conn.Push(msg1)
	conn.Push(msg2)

	buf := make([]byte, 11)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got := string(buf[:n]); got != "hello world" {
		t.Fatalf("Read() = %q, want %q", got, "hello world")
	}
}

func TestPushIgnoresInvalidCiphertext(t *testing.T) {
	_, serverKeys := newTestKeyPair(t)
	conn := New(&stubLink{canSend: true}, serverKeys)

	conn.Push([]byte("bad"))
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	buf := make([]byte, 8)
	n, err := conn.Read(buf)
	if !errors.Is(err, io.EOF) || n != 0 {
		t.Fatalf("Read() = (%d, %v), want (0, EOF)", n, err)
	}
}

func TestWriteEncryptsAndSends(t *testing.T) {
	clientKeys, serverKeys := newTestKeyPair(t)
	ln := &stubLink{canSend: true}
	conn := New(ln, clientKeys)

	n, err := conn.Write([]byte("payload"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len("payload") {
		t.Fatalf("Write() n = %d, want %d", n, len("payload"))
	}
	if len(ln.sent) != 1 {
		t.Fatalf("sent packets = %d, want 1", len(ln.sent))
	}

	got, err := serverKeys.Open(ln.sent[0], []byte(dataRecordAAD))
	if err != nil {
		t.Fatalf("Open(sent) error = %v", err)
	}
	if !bytes.Equal(got, []byte("payload")) {
		t.Fatalf("decrypted payload = %q, want %q", got, "payload")
	}
}

func TestComplementaryKeySetsRoundTripBothPlanes(t *testing.T) {
	clientKeys, serverKeys := newTestKeyPair(t)
	clientLink, serverLink := &loopLink{}, &loopLink{}
	clientLink.peer, serverLink.peer = serverLink, clientLink

	clientData := New(clientLink, clientKeys)
	serverData := New(serverLink, serverKeys)
	clientLink.onData, serverLink.onData = clientData.Push, serverData.Push
	clientControl := NewControl(clientLink, clientKeys)
	serverControl := NewControl(serverLink, serverKeys)

	assertConnRoundTrip(t, clientData, serverData, "client data")
	assertConnRoundTrip(t, serverData, clientData, "server data")
	assertConnRoundTrip(t, clientControl, serverControl, "client control")
	assertConnRoundTrip(t, serverControl, clientControl, "server control")
}

func assertConnRoundTrip(t *testing.T, sender, receiver *Conn, payload string) {
	t.Helper()
	if _, err := sender.Write([]byte(payload)); err != nil {
		t.Fatalf("Write(%q) error = %v", payload, err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(receiver, buf); err != nil {
		t.Fatalf("ReadFull(%q) error = %v", payload, err)
	}
	if string(buf) != payload {
		t.Fatalf("round trip = %q, want %q", buf, payload)
	}
}

func TestReplayRejectedAcrossMuxconnRecreation(t *testing.T) {
	clientKeys, serverKeys := newTestKeyPair(t)
	clientLink := &stubLink{canSend: true}
	clientConn := New(clientLink, clientKeys)
	firstServerConn := New(&stubLink{canSend: true}, serverKeys)
	if _, err := clientConn.Write([]byte("first")); err != nil {
		t.Fatalf("Write(first) error = %v", err)
	}
	firstRecord := clientLink.sent[0]
	firstServerConn.Push(firstRecord)
	assertRead(t, firstServerConn, "first")

	secondServerConn := New(&stubLink{canSend: true}, serverKeys)
	secondServerConn.Push(firstRecord)
	if len(secondServerConn.in) != 0 {
		t.Fatalf("replayed frames queued = %d, want 0", len(secondServerConn.in))
	}
	if _, err := clientConn.Write([]byte("second")); err != nil {
		t.Fatalf("Write(second) error = %v", err)
	}
	secondServerConn.Push(clientLink.sent[1])
	assertRead(t, secondServerConn, "second")
}

func assertRead(t *testing.T, conn *Conn, want string) {
	t.Helper()
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("ReadFull() error = %v", err)
	}
	if string(buf) != want {
		t.Fatalf("ReadFull() = %q, want %q", buf, want)
	}
}

func TestPeerWriteEncryptsAndSendsToPeer(t *testing.T) {
	_, serverKeys := newTestKeyPair(t)
	clientReceiver, err := cryptopkg.NewKeySet([]byte("01234567890123456789012345678901"), cryptopkg.Client)
	if err != nil {
		t.Fatalf("NewKeySet(client receiver) error = %v", err)
	}
	ln := &stubLink{canSend: true}
	conn := NewPeer(ln, serverKeys, "peer-a")

	n, err := conn.Write([]byte("payload"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len("payload") {
		t.Fatalf("Write() n = %d, want %d", n, len("payload"))
	}
	if len(ln.sent) != 0 {
		t.Fatalf("broadcast sent packets = %d, want 0", len(ln.sent))
	}
	if len(ln.peerSent["peer-a"]) != 1 {
		t.Fatalf("peer sent packets = %d, want 1", len(ln.peerSent["peer-a"]))
	}

	got, err := clientReceiver.Open(ln.peerSent["peer-a"][0], []byte(dataRecordAAD))
	if err != nil {
		t.Fatalf("Open(peer sent) error = %v", err)
	}
	if !bytes.Equal(got, []byte("payload")) {
		t.Fatalf("decrypted payload = %q, want %q", got, "payload")
	}
}

func TestWriteWaitsForCanSend(t *testing.T) {
	clientKeys, _ := newTestKeyPair(t)
	start := time.Now()
	readyAt := start.Add(15 * time.Millisecond)
	ln := &stubLink{
		canSendFn: func() bool {
			return time.Now().After(readyAt)
		},
	}
	conn := New(ln, clientKeys)

	if _, err := conn.Write([]byte("payload")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if len(ln.sent) != 1 {
		t.Fatalf("sent packets = %d, want 1", len(ln.sent))
	}
}

func TestWriteReturnsErrClosedWhileWaiting(t *testing.T) {
	clientKeys, _ := newTestKeyPair(t)
	conn := New(&stubLink{canSend: false}, clientKeys)

	done := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("payload"))
		done <- err
	}()

	time.Sleep(10 * time.Millisecond)
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Write() error = %v, want %v", err, ErrClosed)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Write() did not unblock after Close")
	}
}

func TestWriteWrapsSendError(t *testing.T) {
	clientKeys, _ := newTestKeyPair(t)
	conn := New(&stubLink{canSend: true, sendErr: errMuxBoom}, clientKeys)

	_, err := conn.Write([]byte("payload"))
	if err == nil || err.Error() != "send: boom" {
		t.Fatalf("Write() error = %v", err)
	}
}

func TestWriteTimesOutWhenTransportNeverReady(t *testing.T) {
	clientKeys, _ := newTestKeyPair(t)
	conn := New(&stubLink{canSend: false}, clientKeys)
	conn.writeTimeout = 20 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("payload"))
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrWriteTimeout) {
			t.Fatalf("Write() error = %v, want %v", err, ErrWriteTimeout)
		}
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("Write() error = %v, want a net.Error with Timeout() == true", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write() did not time out on a permanently blocked transport")
	}
}

func TestReadCloseErrorIsBothEOFAndErrClosed(t *testing.T) {
	clientKeys, _ := newTestKeyPair(t)
	conn := New(&stubLink{canSend: true}, clientKeys)
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	_, err := conn.Read(make([]byte, 4))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Read() error = %v, want it to wrap io.EOF", err)
	}
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Read() error = %v, want it to wrap %v", err, ErrClosed)
	}
}

func TestCloseRecyclesQueuedFrames(t *testing.T) {
	clientKeys, serverKeys := newTestKeyPair(t)
	conn := New(&stubLink{canSend: true}, serverKeys)
	for range 4 {
		msg, err := clientKeys.Seal([]byte("queued"), []byte(dataRecordAAD))
		if err != nil {
			t.Fatalf("Seal() error = %v", err)
		}
		conn.Push(msg)
	}
	if len(conn.in) != 4 {
		t.Fatalf("queued frames = %d, want 4", len(conn.in))
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if len(conn.in) != 0 {
		t.Fatalf("queued frames after Close = %d, want 0", len(conn.in))
	}
}

func TestPushRateLimitsDecryptFailureLogs(t *testing.T) {
	_, serverKeys := newTestKeyPair(t)
	conn := New(&stubLink{canSend: true}, serverKeys)

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	for range 500 {
		conn.Push([]byte("bad"))
	}

	if got := strings.Count(buf.String(), "decrypt failed"); got != 1 {
		t.Fatalf("decrypt failure log lines = %d, want 1:\n%s", got, buf.String())
	}
}

func TestCloseMakesReadReturnEOF(t *testing.T) {
	clientKeys, _ := newTestKeyPair(t)
	conn := New(&stubLink{canSend: true}, clientKeys)

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4)
		n, err := conn.Read(buf)
		if !errors.Is(err, io.EOF) || n != 0 {
			t.Errorf("Read() = (%d, %v), want (0, EOF)", n, err)
		}
	}()

	time.Sleep(10 * time.Millisecond)
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Read() did not unblock after Close")
	}
}

func TestDecryptStatsTellBadMagicFromAuthFailures(t *testing.T) {
	clientKeys, _ := newTestKeyPair(t)
	conn := New(&stubLink{canSend: true}, clientKeys)
	defer func() { _ = conn.Close() }()

	// Long enough to be a record, but not one: a pre-v2 peer.
	conn.Push(make([]byte, cryptopkg.WireOverhead+8))
	// A real v2 record under another PSK: a peer on another key.
	stranger, err := cryptopkg.NewKeySet([]byte("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"), cryptopkg.Server)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := stranger.Seal([]byte("hello"), []byte(dataRecordAAD))
	if err != nil {
		t.Fatal(err)
	}
	conn.Push(sealed)

	got := conn.DecryptStats()
	if got.BadMagic != 1 || got.AuthFailed != 1 {
		t.Fatalf("DecryptStats = %+v, want BadMagic 1 AuthFailed 1", got)
	}
}
