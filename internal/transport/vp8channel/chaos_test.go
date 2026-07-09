package vp8channel

import (
	"bytes"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// chaosPump is a drop-in replacement for pumpPackets that injects network
// pathology between two kcpRuntimes. cfg drives loss/reorder/delay; all
// three default to "pass through" when zero.
//
// This sits at the same seam as production: kcpConn.WriteTo emits packets
// into `from`; we forward (or not) into `to.deliver()`. Real network
// hardware does the same things at the IP layer.
type chaosCfg struct {
	lossRatio    float64       // 0..1 probability of dropping a packet
	reorderRatio float64       // 0..1 probability of delaying a packet by `reorderHold`
	reorderHold  time.Duration // hold-and-release delay for reordered packets
	latency      time.Duration // base one-way latency applied to every packet
	seed         uint64        // RNG seed; 0 picks 1
}

//nolint:cyclop // chaos pump intentionally has several independent injection paths
func chaosPump(
	t *testing.T,
	stop <-chan struct{},
	from <-chan []byte,
	to *kcpRuntime,
	cfg chaosCfg,
	dropped *atomic.Uint64,
) {
	t.Helper()
	seed := cfg.seed
	if seed == 0 {
		seed = 1
	}
	rng := rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15)) //nolint:gosec // weak RNG is fine for test fixtures

	// Held packets to be released after `reorderHold`.
	type held struct {
		release time.Time
		pkt     []byte
	}
	var holdMu sync.Mutex
	var holdQ []held
	releaseTick := time.NewTicker(2 * time.Millisecond)
	defer releaseTick.Stop()

	forward := func(p []byte) {
		if len(p) > epochHdrLen {
			to.deliver(p[epochHdrLen:])
		}
	}

	for {
		select {
		case <-stop:
			return
		case <-releaseTick.C:
			holdMu.Lock()
			now := time.Now()
			kept := holdQ[:0]
			for _, h := range holdQ {
				if !now.Before(h.release) {
					forward(h.pkt)
					continue
				}
				kept = append(kept, h)
			}
			holdQ = kept
			holdMu.Unlock()
		case pkt := <-from:
			pkt = append([]byte(nil), pkt...) // detach from sender buffer
			if cfg.lossRatio > 0 && rng.Float64() < cfg.lossRatio {
				if dropped != nil {
					dropped.Add(1)
				}
				continue
			}
			if cfg.latency > 0 {
				time.Sleep(cfg.latency)
			}
			if cfg.reorderRatio > 0 && cfg.reorderHold > 0 && rng.Float64() < cfg.reorderRatio {
				holdMu.Lock()
				holdQ = append(holdQ, held{release: time.Now().Add(cfg.reorderHold), pkt: pkt})
				holdMu.Unlock()
				continue
			}
			forward(pkt)
		}
	}
}

// runChaosLoopback wires a chaotic channel A↔B, sends msgs from A, and
// verifies B receives them in order. Returns observed receive duration.
func runChaosLoopback(t *testing.T, msgs [][]byte, cfg chaosCfg, timeout time.Duration) (time.Duration, uint64) {
	t.Helper()

	a2b := make(chan []byte, 1024)
	b2a := make(chan []byte, 1024)

	cb, doneB, getRecv := buildReceiver(len(msgs))

	rtA, err := startKCP(a2b, nil, testEpochHdr(1), 0)
	if err != nil {
		t.Fatalf("startKCP A: %v", err)
	}
	defer rtA.close()

	rtB, err := startKCP(b2a, cb, testEpochHdr(2), 0)
	if err != nil {
		t.Fatalf("startKCP B: %v", err)
	}
	defer rtB.close()

	stop := make(chan struct{})
	defer close(stop)

	var droppedAB, droppedBA atomic.Uint64
	go chaosPump(t, stop, a2b, rtB, cfg, &droppedAB)
	// Return path stays clean by default - KCP ACKs must come back reliably
	// for fair loss measurement; loss on one direction is enough to stress.
	go chaosPump(t, stop, b2a, rtA, chaosCfg{}, &droppedBA)

	start := time.Now()
	for _, m := range msgs {
		if err := rtA.send(m); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	select {
	case <-doneB:
	case <-time.After(timeout):
		got := getRecv()
		t.Fatalf("timeout: got %d/%d messages, dropped A->B=%d", len(got), len(msgs), droppedAB.Load())
	}
	dur := time.Since(start)
	checkMessages(t, getRecv(), msgs)
	return dur, droppedAB.Load()
}

// TestKCPSurvivesModeratePacketLoss confirms KCP's ARQ delivers all
// messages despite ~10% packet loss. This is the headline regression
// guard: if anything in vp8channel's KCP wiring (window size, retransmit
// pacing, conv stability) regresses, this test will flake or time out.
func TestKCPSurvivesModeratePacketLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping chaos test in -short mode")
	}
	msgs := [][]byte{
		[]byte("alpha"),
		bytes.Repeat([]byte("B"), 2000),
		bytes.Repeat([]byte("C"), 8000),
		bytes.Repeat([]byte("D"), 20000),
	}
	dur, dropped := runChaosLoopback(t, msgs, chaosCfg{lossRatio: 0.10, seed: 0xC0FFEE}, 20*time.Second)
	t.Logf("delivered %d msgs in %s with %d packets dropped (10%% loss)", len(msgs), dur, dropped)
	if dropped == 0 {
		t.Fatal("chaos pump did not drop any packets - loss injection broken")
	}
}

// TestKCPSurvivesReorder confirms KCP delivers messages in order even when
// ~20% of packets are arbitrarily held and re-released. videochannel does
// NOT tolerate this (it uses sequence+CRC reassembly that drops on reorder),
// but KCP under vp8channel must.
func TestKCPSurvivesReorder(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping chaos test in -short mode")
	}
	msgs := [][]byte{
		bytes.Repeat([]byte("R"), 4000),
		bytes.Repeat([]byte("S"), 12000),
		bytes.Repeat([]byte("T"), 30000),
	}
	dur, _ := runChaosLoopback(t, msgs, chaosCfg{
		reorderRatio: 0.20,
		reorderHold:  30 * time.Millisecond,
		seed:         0xBEEF,
	}, 15*time.Second)
	t.Logf("reorder-tolerant delivery in %s", dur)
}

// TestKCPRecoversFromBurstLoss simulates a complete blackout for ~200ms
// then full restoration. This mirrors a real connectivity blip: the
// transport should not give up; KCP should resend everything queued
// during the blackout once the path comes back.
//
//nolint:cyclop // setup + gated pump + assertions naturally branch several ways
func TestKCPRecoversFromBurstLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping chaos test in -short mode")
	}
	msgs := [][]byte{
		bytes.Repeat([]byte("X"), 1500),
		bytes.Repeat([]byte("Y"), 1500),
		bytes.Repeat([]byte("Z"), 1500),
	}

	a2b := make(chan []byte, 1024)
	b2a := make(chan []byte, 1024)
	cb, doneB, getRecv := buildReceiver(len(msgs))

	rtA, err := startKCP(a2b, nil, testEpochHdr(1), 0)
	if err != nil {
		t.Fatalf("startKCP A: %v", err)
	}
	defer rtA.close()
	rtB, err := startKCP(b2a, cb, testEpochHdr(2), 0)
	if err != nil {
		t.Fatalf("startKCP B: %v", err)
	}
	defer rtB.close()

	stop := make(chan struct{})
	defer close(stop)

	var blackout atomic.Bool
	gate := func(stop <-chan struct{}, from <-chan []byte, to *kcpRuntime) {
		for {
			select {
			case <-stop:
				return
			case pkt := <-from:
				if blackout.Load() {
					continue // drop everything during blackout
				}
				if len(pkt) > epochHdrLen {
					to.deliver(pkt[epochHdrLen:])
				}
			}
		}
	}
	go gate(stop, a2b, rtB)
	go gate(stop, b2a, rtA)

	// Begin in blackout, send messages, wait, then lift.
	blackout.Store(true)
	for _, m := range msgs {
		if err := rtA.send(m); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	blackout.Store(false)

	select {
	case <-doneB:
	case <-time.After(15 * time.Second):
		got := getRecv()
		t.Fatalf("did not recover from blackout: got %d/%d", len(got), len(msgs))
	}
	checkMessages(t, getRecv(), msgs)
}

// TestKCPThroughputBaseline establishes a perfect-channel throughput floor.
// Not an assertion - if this number regresses meaningfully on the same
// hardware, something changed in KCP options (window size, MTU, tick).
func TestKCPThroughputBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping throughput baseline in -short mode")
	}
	const payloadSize = 8000
	const messages = 50
	msgs := make([][]byte, messages)
	for i := range msgs {
		msgs[i] = bytes.Repeat([]byte{byte('A' + (i % 26))}, payloadSize)
	}
	dur, _ := runChaosLoopback(t, msgs, chaosCfg{}, 30*time.Second)
	total := messages * payloadSize
	mbPerSec := float64(total) / dur.Seconds() / (1 << 20)
	t.Logf("baseline: %d bytes in %s = %.2f MiB/s", total, dur, mbPerSec)
}
