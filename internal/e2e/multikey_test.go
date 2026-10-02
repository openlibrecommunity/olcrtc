package e2e

import (
	"bufio"
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/client"
	"github.com/openlibrecommunity/olcrtc/internal/server"
)

const (
	altKeyHex = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	badKeyHex = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
)

// startRingTunnel runs a server holding the ring and a client holding
// clientKey, and reports whether the client became ready within readyBudget.
func startRingTunnel(t *testing.T, ring []string, clientKey string, readyBudget time.Duration) (string, bool) {
	t.Helper()
	providerName, room := registerMemoryProvider(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	socksAddr := freeLocalAddr(ctx, t)
	go func() {
		_ = server.Run(ctx, server.Config{
			Transport: transportData, Provider: providerName, RoomURL: testRoom,
			Keys: ring, DNSServer: localDNSServer,
		})
	}()
	room.waitConnected(t, 1)
	ready := make(chan struct{})
	go func() {
		_ = client.RunWithReady(ctx, client.Config{
			Transport: transportData, Provider: providerName, RoomURL: testRoom,
			KeyHex: clientKey, DeviceID: testClientDeviceID, LocalAddr: socksAddr, DNSServer: localDNSServer,
		}, func() { close(ready) })
	}()
	select {
	case <-ready:
		return socksAddr, true
	case <-time.After(readyBudget):
		return socksAddr, false
	}
}

func TestMultiKeySecondRingKeyPairsAndRelays(t *testing.T) {
	echoAddr := startEchoServer(t)
	socksAddr, ready := startRingTunnel(t, []string{testKeyHex, altKeyHex}, altKeyHex, 20*time.Second)
	if !ready {
		t.Fatal("client holding the second ring key never became ready")
	}
	conn := connectViaSOCKS(t, socksAddr, echoAddr)
	defer func() { _ = conn.Close() }()
	payload := []byte("olcrtc-multikey\n")
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil || !bytes.Equal(line, payload) {
		t.Fatalf("echo = %q, %v", line, err)
	}
}

func TestMultiKeyUnknownKeyNeverPairs(t *testing.T) {
	echoAddr := startEchoServer(t)
	socksAddr, ready := startRingTunnel(t, []string{testKeyHex, altKeyHex}, badKeyHex, 3*time.Second)
	if ready {
		// The SOCKS listener opens only after the handshake; a client outside
		// the ring must never get there.
		conn := connectViaSOCKS(t, socksAddr, echoAddr)
		_ = conn.Close()
		t.Fatal("a key outside the ring paired")
	}
}
