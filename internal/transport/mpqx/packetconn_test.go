package mpqx_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	mrand "math/rand"
	"net"
	"sync"
	"testing"
	"time"

	core "github.com/SolverNA/mpq-brutal/core"
	"github.com/openlibrecommunity/olcrtc/internal/transport/mpqx"
)

// memTransport is a tiny in-memory carrier end. Send ships a message to the
// peer end's onData; onData is set after construction (mirroring the way real
// transports receive their OnData through Config at build time). It implements
// mpqx.Sender via Send.
type memTransport struct {
	mu     sync.Mutex
	peer   *memTransport
	onData func([]byte)
}

// newMemPair returns two connected ends: Send on one delivers to the other's
// onData. Delivery hops through a goroutine so Send never reenters onData on
// the calling stack (as a real async carrier behaves).
func newMemPair() (*memTransport, *memTransport) {
	a := &memTransport{}
	b := &memTransport{}
	a.peer = b
	b.peer = a
	return a, b
}

func (t *memTransport) setOnData(cb func([]byte)) {
	t.mu.Lock()
	t.onData = cb
	t.mu.Unlock()
}

func (t *memTransport) Send(data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	peer := t.peer
	go func() {
		peer.mu.Lock()
		cb := peer.onData
		peer.mu.Unlock()
		if cb != nil {
			cb(cp)
		}
	}()
	return nil
}

// generateTLSConfig builds a server tls.Config plus the SPKI pin of its key, so
// the client can pin it with core.PinnedClientTLS.
func generateTLSConfig(t *testing.T) (*tls.Config, string) {
	t.Helper()
	cert, err := core.GenerateSelfSigned([]string{"localhost"})
	if err != nil {
		t.Fatalf("GenerateSelfSigned: %v", err)
	}
	pin, err := core.SPKIPinFromCert(cert)
	if err != nil {
		t.Fatalf("SPKIPinFromCert: %v", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, pin
}

// TestMPQOverCarrier proves that mpq-brutal runs over a transport.Transport
// carrier via the mpqx.PacketConn adapter: it stands up an mpq server on the B
// end and an mpq client on the A end (single path), pushes a few hundred KB in
// each direction, verifies byte-for-byte equality, and confirms neither side's
// Close hangs.
func TestMPQOverCarrier(t *testing.T) {
	endA, endB := newMemPair()

	// One adapter per carrier end; wire each end's inbound to its adapter.
	pcA := mpqx.New(endA)
	pcB := mpqx.New(endB)
	endA.setOnData(pcA.Deliver)
	endB.setOnData(pcB.Deliver)

	serverTLS, pin := generateTLSConfig(t)

	ln, err := core.Listen(core.Config{
		TLSConfig:     serverTLS,
		ServerConn:    pcB,
		ExpectedPaths: 1,
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srvCh := make(chan *core.Session, 1)
	accErr := make(chan error, 1)
	go func() {
		s, err := ln.Accept(ctx)
		if err != nil {
			accErr <- err
			return
		}
		srvCh <- s
	}()

	// Client: single path over pcA; DialPath hands back the adapter and the
	// synthetic remote address.
	dialPath := func(_ context.Context, _ int, _ string) (net.PacketConn, net.Addr, error) {
		return pcA, mpqx.RemoteAddr(), nil
	}
	client, err := core.Dial(ctx, core.Config{
		DialPath:      dialPath,
		LocalAddrs:    []string{"mpqx-client-0"},
		ExpectedPaths: 1,
		TLSConfig:     core.PinnedClientTLS("localhost", pin, nil),
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	var server *core.Session
	select {
	case server = <-srvCh:
	case err := <-accErr:
		t.Fatalf("Accept: %v", err)
	case <-ctx.Done():
		t.Fatal("timeout waiting for Accept")
	}
	defer server.Close()

	const total = 512 << 10 // 512 KB each direction

	// client -> server
	up := make([]byte, total)
	mrand.New(mrand.NewSource(1)).Read(up)
	upErr := make(chan error, 1)
	go func() {
		_, err := client.Write(up)
		upErr <- err
	}()
	upRecv := make([]byte, total)
	if _, err := io.ReadFull(server, upRecv); err != nil {
		t.Fatalf("server read: %v", err)
	}
	if err := <-upErr; err != nil {
		t.Fatalf("client write: %v", err)
	}
	if !bytes.Equal(up, upRecv) {
		t.Fatal("upstream: received bytes differ from sent")
	}

	// server -> client
	down := make([]byte, total)
	mrand.New(mrand.NewSource(2)).Read(down)
	downErr := make(chan error, 1)
	go func() {
		_, err := server.Write(down)
		downErr <- err
	}()
	downRecv := make([]byte, total)
	if _, err := io.ReadFull(client, downRecv); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if err := <-downErr; err != nil {
		t.Fatalf("server write: %v", err)
	}
	if !bytes.Equal(down, downRecv) {
		t.Fatal("downstream: received bytes differ from sent")
	}

	// Close of both mpq sessions must not hang (relies on the adapter's
	// SetReadDeadline waking the quic read loop). Guard with a watchdog.
	closed := make(chan struct{})
	go func() {
		_ = client.Close()
		_ = server.Close()
		_ = ln.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung")
	}
}

// TestReadFromDeadlineAndClose covers the adapter in isolation: a read deadline
// unblocks ReadFrom with a timeout error, and Close unblocks a pending ReadFrom
// with net.ErrClosed.
func TestReadFromDeadlineAndClose(t *testing.T) {
	// Deadline path.
	pc := mpqx.New(nil)
	_ = pc.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	buf := make([]byte, 16)
	start := time.Now()
	_, _, err := pc.ReadFrom(buf)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("expected net.Error with Timeout()==true, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("ReadFrom did not honor deadline promptly: %v", time.Since(start))
	}

	// Close path: Close must unblock a blocked ReadFrom.
	pc2 := mpqx.New(nil)
	done := make(chan error, 1)
	go func() {
		_, _, err := pc2.ReadFrom(make([]byte, 16))
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = pc2.Close()
	select {
	case err := <-done:
		if err != net.ErrClosed {
			t.Fatalf("expected net.ErrClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock ReadFrom")
	}
}
