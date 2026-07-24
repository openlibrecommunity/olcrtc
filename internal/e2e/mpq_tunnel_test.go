package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/client"
	"github.com/openlibrecommunity/olcrtc/internal/server"
)

// startTunnelProto brings up an in-process server+client tunnel over the memory
// datachannel carrier using the given TransportProto ("legacy" or "mpq"). It
// mirrors startTunnel but threads the protocol selector through both ends so the
// same SOCKS/echo assertions can validate either stack.
func startTunnelProto(t *testing.T, proto string) *tunnelRuntime {
	t.Helper()

	carrierName, room := registerMemoryCarrier(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	socksAddr := freeLocalAddr(ctx, t)

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Run(ctx, server.Config{
			Transport:      transportData,
			TransportProto: proto,
			Carrier:        carrierName,
			RoomURL:        testRoom,
			KeyHex:         testKeyHex,
			DNSServer:      localDNSServer,
		})
	}()
	room.waitConnected(t, 1)

	ready := make(chan struct{})
	clientErr := make(chan error, 1)
	go func() {
		clientErr <- client.RunWithReady(ctx, client.Config{
			Transport:      transportData,
			TransportProto: proto,
			Carrier:        carrierName,
			RoomURL:        testRoom,
			KeyHex:         testKeyHex,
			DeviceID:       testClientDeviceID,
			LocalAddr:      socksAddr,
			DNSServer:      localDNSServer,
		}, func() { close(ready) })
	}()
	// The mpq path adds a QUIC handshake on top of the carrier bring-up, so
	// allow a slightly longer budget than the plain datachannel default.
	waitForReadyWithin(t, ready, 10*time.Second)

	return &tunnelRuntime{
		socksAddr: socksAddr,
		room:      room,
		cancel:    cancel,
		serverErr: serverErr,
		clientErr: clientErr,
		stopWait:  5 * time.Second,
	}
}

// startLossyMPQTunnel brings up an in-process server+client mpq tunnel over a
// memory carrier that DROPS and REORDERS carrier messages. It proves the tunnel
// survives an unreliable/unordered carrier: with the carrier doing no
// retransmission or ordering, QUIC (inside mpq) is the sole reliability layer
// and must recover the loss on its own — i.e. there is no double-reliability.
func startLossyMPQTunnel(t *testing.T, dropProb, reorderProb float64, maxDelay time.Duration) *tunnelRuntime {
	t.Helper()

	carrierName, room := registerLossyMemoryCarrier(t, dropProb, reorderProb, maxDelay)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	socksAddr := freeLocalAddr(ctx, t)

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Run(ctx, server.Config{
			Transport:      transportData,
			TransportProto: "mpq",
			Carrier:        carrierName,
			RoomURL:        testRoom,
			KeyHex:         testKeyHex,
			DNSServer:      localDNSServer,
		})
	}()
	room.waitConnected(t, 1)

	ready := make(chan struct{})
	clientErr := make(chan error, 1)
	go func() {
		clientErr <- client.RunWithReady(ctx, client.Config{
			Transport:      transportData,
			TransportProto: "mpq",
			Carrier:        carrierName,
			RoomURL:        testRoom,
			KeyHex:         testKeyHex,
			DeviceID:       testClientDeviceID,
			LocalAddr:      socksAddr,
			DNSServer:      localDNSServer,
		}, func() { close(ready) })
	}()
	// A lossy carrier stretches both the QUIC handshake and the smux/control
	// handshake (each dropped packet waits out a retransmit timer), so budget
	// generously.
	waitForReadyWithin(t, ready, 30*time.Second)

	return &tunnelRuntime{
		socksAddr: socksAddr,
		room:      room,
		cancel:    cancel,
		serverErr: serverErr,
		clientErr: clientErr,
		stopWait:  10 * time.Second,
	}
}

// TestMPQTunnelLossyCarrier drives real data through the mpq SOCKS5 tunnel over
// a carrier that drops ~4% of messages and reorders ~10% of them. A byte-exact
// echo of a large random blob proves QUIC recovered every dropped/reordered
// carrier message: the carrier itself does no retransmission or reordering, so
// all reliability came from QUIC (no double-reliability with the carrier).
func TestMPQTunnelLossyCarrier(t *testing.T) {
	echoAddr := startEchoServer(t)
	rt := startLossyMPQTunnel(t, 0.04, 0.10, 5*time.Millisecond)
	defer rt.stop(t)

	conn := connectViaSOCKS(t, rt.socksAddr, echoAddr)
	defer func() { _ = conn.Close() }()

	// Small line first: exercises the round-trip on the small-write path.
	line := []byte("olcrtc-mpq-lossy-e2e-payload\n")
	if _, err := conn.Write(line); err != nil {
		t.Fatalf("write tunneled line: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	got := make([]byte, len(line))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read tunneled line: %v", err)
	}
	if !bytes.Equal(got, line) {
		t.Fatalf("echo line = %q, want %q", got, line)
	}

	// Bulk blob: forces many carrier messages, so with ~4% loss QUIC must
	// retransmit repeatedly and still hand up a byte-exact, in-order stream.
	payload := make([]byte, 256*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("gen payload: %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(60 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	echoed := make([]byte, len(payload))
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(conn, echoed)
		done <- err
	}()
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write tunneled payload: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("read tunneled payload: %v", err)
	}
	if !bytes.Equal(echoed, payload) {
		t.Fatalf("echo payload mismatch (%d bytes)", len(payload))
	}
}

// TestClientServerSOCKSTunnelTransportProto proves the SOCKS5 tunnel carries
// real data end-to-end for both the legacy muxconn+smux stack and the mpq-brutal
// bonding QUIC session, over the same in-process memory datachannel carrier.
func TestClientServerSOCKSTunnelTransportProto(t *testing.T) {
	for _, proto := range []string{"legacy", "mpq"} {
		proto := proto
		t.Run(proto, func(t *testing.T) {
			echoAddr := startEchoServer(t)
			rt := startTunnelProto(t, proto)
			defer rt.stop(t)

			conn := connectViaSOCKS(t, rt.socksAddr, echoAddr)
			defer func() { _ = conn.Close() }()

			// A short line first: exercises the small-write / round-trip path.
			line := []byte("olcrtc-" + proto + "-e2e-payload\n")
			if _, err := conn.Write(line); err != nil {
				t.Fatalf("write tunneled line: %v", err)
			}
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatalf("set read deadline: %v", err)
			}
			got := make([]byte, len(line))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatalf("read tunneled line: %v", err)
			}
			if !bytes.Equal(got, line) {
				t.Fatalf("echo line = %q, want %q", got, line)
			}

			// A larger blob: exercises bulk data through the tunnel and confirms
			// the mpq session's reliable ordered byte stream reassembles intact.
			payload := make([]byte, 128*1024)
			if _, err := rand.Read(payload); err != nil {
				t.Fatalf("gen payload: %v", err)
			}
			if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
				t.Fatalf("set deadline: %v", err)
			}
			echoed := make([]byte, len(payload))
			done := make(chan error, 1)
			go func() {
				_, err := io.ReadFull(conn, echoed)
				done <- err
			}()
			if _, err := conn.Write(payload); err != nil {
				t.Fatalf("write tunneled payload: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("read tunneled payload: %v", err)
			}
			if !bytes.Equal(echoed, payload) {
				t.Fatalf("echo payload mismatch (%d bytes)", len(payload))
			}
		})
	}
}
