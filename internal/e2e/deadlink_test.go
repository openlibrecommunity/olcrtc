package e2e

import (
	"context"
	"flag"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/client"
	"github.com/openlibrecommunity/olcrtc/internal/control"
	"github.com/openlibrecommunity/olcrtc/internal/server"
)

// deadLinkEnabled gates TestDeadLinkDetectionWindow. It runs a real
// in-process client/server tunnel over the memory provider, then silently
// black-holes the provider (link stays "up", frames vanish) and measures how
// long the client takes to notice the dead link and trigger a reconnect.
//
// This is the exact failure mode behind the jitsi/datachannel regression:
// the peer leaves but the WebRTC PC has not torn down, so the only signal is
// the control-stream liveness timeout. The bad commit widened that timeout to
// 120s globally; the fix scopes the long window to ControlPlane transports so
// datachannel detects a dead link on the conservative ~30s smux keepalive.
var deadLinkEnabled = flag.Bool(
	"olcrtc.deadlink",
	false,
	"run TestDeadLinkDetectionWindow (measures dead-link detection latency)",
)

// TestDeadLinkDetectionWindow proves the datachannel dead-link detection
// window returned to the conservative band after the fix. It fails if the
// client does not detect the silently-dead provider and reconnect within the
// expected window (well under the buggy 120s, comfortably above the
// conservative 30s keepalive timeout).
func TestDeadLinkDetectionWindow(t *testing.T) {
	if !*deadLinkEnabled {
		t.Skip("dead-link test disabled; pass -olcrtc.deadlink to enable")
	}

	const (
		transportName = transportData // datachannel: no isolated control plane
		// The conservative smux keepalive timeout is 30s (interval 10s). Allow
		// generous headroom for scheduling + a probe cycle, but stay well below
		// the buggy 120s so the bad commit fails this bound.
		detectionBudget = 75 * time.Second
		setupBudget     = 30 * time.Second
	)

	providerName, room := registerMemoryProvider(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	socksAddr := freeLocalAddr(ctx, t)

	// Count reconnects observed by the client's health hook. A dead-link
	// detection manifests as a reconnect (control loop ends -> handleReconnect).
	var reconnects atomic.Uint64
	detected := make(chan struct{}, 1)
	onHealth := func(st control.Status) {
		if st.Reconnects > 0 || st.UnhealthyEvents > 0 {
			if reconnects.Swap(st.Reconnects) == 0 {
				select {
				case detected <- struct{}{}:
				default:
				}
			}
		}
	}

	// The session-open callback is the mobile host's view of this same event: it
	// must fire for the initial session and again for the reconnected one.
	var sessionOpens atomic.Int32
	onSessionOpen := func(string) { sessionOpens.Add(1) }

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Run(ctx, server.Config{
			Transport: transportName,
			Provider:  providerName,
			RoomURL:   testRoom,
			KeyHex:    testKeyHex,
			DNSServer: localDNSServer,
		})
	}()
	room.waitConnected(t, 1)

	ready := make(chan struct{})
	clientErr := make(chan error, 1)
	go func() {
		clientErr <- client.RunWithReady(ctx, client.Config{
			Transport:     transportName,
			Provider:      providerName,
			RoomURL:       testRoom,
			KeyHex:        testKeyHex,
			DeviceID:      testClientDeviceID,
			LocalAddr:     socksAddr,
			DNSServer:     localDNSServer,
			OnHealth:      onHealth,
			OnSessionOpen: onSessionOpen,
		}, func() { close(ready) })
	}()
	waitForReadyWithin(t, ready, setupBudget)

	// Let the control stream settle into a healthy steady state first.
	time.Sleep(2 * time.Second)

	// Silently kill the provider: link stays up, frames are dropped. Only the
	// control-stream liveness timeout can detect this.
	t.Logf("[deadlink] enabling blackhole at %s", time.Now().Format("15:04:05"))
	start := time.Now()
	room.enableBlackhole()

	select {
	case <-detected:
		elapsed := time.Since(start)
		t.Logf("[deadlink] dead link detected + reconnect in %s", elapsed.Round(time.Millisecond))
		if elapsed > detectionBudget {
			t.Fatalf("dead-link detection took %s, want <= %s (regression: liveness window too wide)",
				elapsed.Round(time.Second), detectionBudget)
		}
	case <-time.After(detectionBudget):
		t.Fatalf("dead link NOT detected within %s (regression: client treats dead provider as alive)",
			detectionBudget)
	}

	deadline := time.Now().Add(5 * time.Second)
	for sessionOpens.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := sessionOpens.Load(); got < 2 {
		t.Fatalf("session-open callbacks = %d, want >= 2 (initial + reconnect)", got)
	}
}
