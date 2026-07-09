package vp8channel

import (
	"bytes"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"
)

// corruptPump forwards packets from `from` into `to.deliver`, flipping a byte
// in the KCP body of a fraction of them. It models a carrier (an SFU that may
// transcode our fake-VP8 stream) that perturbs payload bytes without dropping
// the packet outright. KCP runs with block=nil, so before the wire CRC was
// added a corrupt-but-parseable segment rode through as valid in-order data
// and broke the muxconn AEAD above it (issue #109).
func corruptPump(
	stop <-chan struct{},
	from <-chan []byte,
	to *kcpRuntime,
	corruptRatio float64,
	seed uint64,
	corrupted *atomic.Uint64,
) {
	if seed == 0 {
		seed = 1
	}
	rng := rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15)) //nolint:gosec // weak RNG fine for test fixtures
	for {
		select {
		case <-stop:
			return
		case pkt := <-from:
			if len(pkt) <= epochHdrLen {
				continue
			}
			body := append([]byte(nil), pkt[epochHdrLen:]...)
			// Flip a byte in the KCP-packet region, but leave the trailing
			// CRC intact so the corruption is what the CRC must catch.
			if len(body) > wireCRCLen+1 && rng.Float64() < corruptRatio {
				i := rng.IntN(len(body) - wireCRCLen)
				body[i] ^= 0xFF
				if corrupted != nil {
					corrupted.Add(1)
				}
			}
			to.deliver(body)
		}
	}
}

// TestKCPDropsCarrierCorruptedPackets is the issue #109 regression guard. With
// ~15% of packets corrupted in flight, every message must still arrive intact:
// the wire CRC drops the corrupt segments and KCP retransmits them. Without the
// CRC, corrupt bytes reach the receiver and checkMessages fails - exactly the
// "chacha20poly1305: message authentication failed" path one layer up.
func TestKCPDropsCarrierCorruptedPackets(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integrity chaos test in -short mode")
	}
	msgs := [][]byte{
		[]byte("alpha"),
		bytes.Repeat([]byte("B"), 2000),
		bytes.Repeat([]byte("C"), 8000),
		bytes.Repeat([]byte("D"), 20000),
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

	var corrupted atomic.Uint64
	go corruptPump(stop, a2b, rtB, 0.15, 0xD15EA5E, &corrupted)
	// Return path stays clean so ACKs flow back reliably.
	go pumpPackets(stop, b2a, rtA)

	for _, m := range msgs {
		if err := rtA.send(m); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	select {
	case <-doneB:
	case <-time.After(20 * time.Second):
		got := getRecv()
		t.Fatalf("timeout: got %d/%d messages, corrupted=%d", len(got), len(msgs), corrupted.Load())
	}
	checkMessages(t, getRecv(), msgs)
	if corrupted.Load() == 0 {
		t.Fatal("corrupt pump flipped no bytes - corruption injection broken")
	}
	t.Logf("delivered %d msgs intact despite %d corrupted packets", len(msgs), corrupted.Load())
}
