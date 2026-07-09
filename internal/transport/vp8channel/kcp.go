// Package vp8channel provides byte transport over VP8 video frames using KCP.
/*
ЯНДЕКС РАБ ИЛИ КТО ТЫ НАУЙ СУКА КАРОЧЕ ХУЙЛО КЕБАНОГЕ ИЗ ЯНДЕКС ЧИТАЮЩИЙ ЭТО
МЫ ОСЕМЕНИМ ВТОЮ МАМАШКУ МЕРТВУЮ
*/
package vp8channel

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	kcp "github.com/xtaci/kcp-go/v5"
	"golang.org/x/time/rate"
)

// Both peers establish a KCP session with the same convid. KCP does not
// require a handshake - packets are matched by conv field, so a static
// constant gives us a symmetrical P2P setup.
const kcpConvID = 0xC0FFEE01

// KCP tuning targets a lossy, bursty carrier (VP8 over an SFU). The defaults
// are TCP-like and recover slowly after burst losses.
const (
	// kcp-go hardcodes mtuLimit=1500, so SetMtu() above this is silently
	// clamped. Stay below that with headroom for KCP overhead (24 bytes).
	kcpMTU = 1400

	// Send/receive window in segments. Bulk data runs on its own KCP session,
	// isolated from the control plane (ping/pong has a separate startKCP and is
	// drained with priority by writerLoop), so a large data window no longer
	// starves control liveness the way it did before that split (issue #95).
	// One VP8 frame can carry many KCP segments and ACKs only trickle back at
	// frame cadence, so a generous window is what keeps the policed path full
	// and lets throughput reach the SFU's real ceiling (~10 Mbit on Telemost)
	// instead of being clamped to a fraction of it.
	kcpSndWnd = 4096
	kcpRcvWnd = 4096

	// Length prefix for our message framing on top of KCP stream mode.
	// We use stream mode because UDPSession.Write fragments messages > MSS
	// outside of kcp.Send, which destroys the frg field that message mode
	// relies on for boundary preservation. Adding our own length-prefix
	// framing sidesteps that bug entirely.
	kcpLenPrefix = 4

	// Hard cap on a single message. Anything larger would require an
	// unbounded reassembly buffer on the receiver and is almost certainly
	// a protocol error upstream.
	kcpMaxMessage = 8 * 1024 * 1024
)

// Brutal congestion-control tuning (Hysteria "brutal" model). Enabled per
// data-KCP session only when a target rate is configured (BrutalKbps > 0).
//
// Pacing is NOT applied inside KCP (kcp-go's SetRateLimit was removed: on this
// carrier's aggressive 4096-segment window it clamped ACK/flush too and stalled
// the whole session). Instead the token bucket lives on the outbound→writerLoop
// boundary (see kcpRuntime.pace), which paces the VP8 samples leaving the queue
// and lets KCP feel congestion naturally through its own send-window back-
// pressure as the outbound channel fills.
const (
	// brutalInterval is how often the loss compensator re-measures the path
	// and re-applies the send rate limit.
	brutalInterval = 2 * time.Second
	// brutalMaxLossRate clamps the measured loss fraction so a pathological
	// reading cannot drive the effective rate arbitrarily high.
	brutalMaxLossRate = 0.8
	// brutalMaxRateMul caps the effective rate at target*mul; combined with
	// the loss clamp this bounds escalation to a safe multiple of the target.
	brutalMaxRateMul = 5
	// brutalMinBurst is the floor for the token-bucket burst (bytes). It must
	// stay >= defaultMaxPayloadSize so a single maximum-size VP8 sample never
	// exceeds the burst — rate.Limiter.WaitN(n) errors out immediately when
	// n > burst, which would bypass pacing entirely.
	brutalMinBurst = 64 * 1024
)

// brutalEffectiveBps computes the Hysteria "brutal" compensated send rate:
// effective = target / (1 - loss), so that goodput holds at the target even
// as a fraction `loss` of packets is lost. Inputs and output are bytes/sec.
// Clamps: loss is bounded to [0, brutalMaxLossRate]; the result is bounded to
// [target, target*brutalMaxRateMul]. A non-positive target disables scaling.
//
// The function is fully NaN/Inf-safe: a non-finite lossRate (e.g. the 0/0 = NaN
// produced by an empty measurement window with no delivered and no dropped
// packets) is treated as zero loss and the target is returned unchanged. This
// matters because ordinary `<`/`>` comparisons against NaN are always false, so
// without an explicit IsNaN guard a NaN would slip past every clamp and yield
// eff = target/(1-NaN) = NaN. Feeding rate.Limit(NaN) (or a zero rate) into the
// outbound pacer would install a bucket that never refills and stall the send
// path outright (0 bytes flow) — the exact failure this guard prevents. When
// brutal is enabled the result is therefore guaranteed to be in
// [target, target*brutalMaxRateMul] and can never be 0 or non-finite.
func brutalEffectiveBps(targetBps int, lossRate float64) int {
	if targetBps <= 0 {
		return targetBps
	}
	// Any non-finite or negative loss reading (including the 0/0 NaN of an empty
	// window) collapses to zero loss, so eff falls back to the target rate.
	if math.IsNaN(lossRate) || math.IsInf(lossRate, 0) || lossRate < 0 {
		lossRate = 0
	}
	if lossRate > brutalMaxLossRate {
		lossRate = brutalMaxLossRate
	}
	// loss is now clamped to [0, brutalMaxLossRate] (<= 0.8), so (1-loss) is in
	// [0.2, 1] and eff cannot be NaN/Inf; the IsNaN check below is belt-and-
	// braces so the return value is provably finite for any input.
	eff := float64(targetBps) / (1 - lossRate)
	maxEff := float64(targetBps) * brutalMaxRateMul
	if math.IsNaN(eff) || eff > maxEff {
		eff = maxEff
	}
	if eff < float64(targetBps) {
		eff = float64(targetBps)
	}
	return int(eff)
}

// ErrKCPMessageTooLarge is returned by send when the message exceeds
// kcpMaxMessage.
var ErrKCPMessageTooLarge = errors.New("vp8channel: kcp message exceeds maximum size")

// kcpRuntime owns the KCP session and the goroutine that pumps reassembled
// messages from KCP up to cfg.OnData.
type kcpRuntime struct {
	conn     *kcpConn
	sess     *kcp.UDPSession
	readDone chan struct{}
	stopCh   chan struct{} // closed by close() to stop background goroutines (brutal loop)
	writeMu  sync.Mutex    // serializes length-prefix + payload writes

	// limiter is the outbound-layer token bucket for Hysteria "brutal" pacing,
	// nil when brutal is disabled (brutalBps == 0). It rate-limits the bytes of
	// VP8 samples leaving this session's outbound queue (see pace); it never
	// touches KCP's send window directly. pacerCtx is cancelled by close() so a
	// blocked WaitN unblocks promptly on shutdown.
	limiter     *rate.Limiter
	pacerCtx    context.Context
	pacerCancel context.CancelFunc

	closeOnce sync.Once
}

// startKCP builds a KCP session over the vp8channel carrier. brutalBps enables
// Hysteria "brutal" pacing when > 0 (bytes/sec target); pass 0 to keep the
// legacy full-window behaviour with no rate limiter. Only the bulk data-KCP
// should pass a non-zero brutalBps; the control-KCP is low-volume and must not
// be paced. When enabled the pacing is applied on the outbound→writerLoop
// boundary via kcpRuntime.pace, NOT inside KCP.
func startKCP(out chan<- []byte, onData func([]byte), epochHdr [epochHdrLen]byte, brutalBps int) (*kcpRuntime, error) {
	c := newKCPConn(out, inboundQueueSize, epochHdr)

	sess, err := kcp.NewConn3(kcpConvID, fakeUDPAddr(), nil, 0, 0, c)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("kcp new conn: %w", err)
	}

	// nodelay=1, interval=5ms, fast resend=2, congestion control OFF (nc=1).
	// The frame ticker already paces emission at the VP8 frame cadence, so the
	// 5ms KCP tick just keeps scheduling latency low; a slower tick only adds
	// dead time before retransmits and ACKs. nc=1 disables KCP's loss-based
	// congestion control because the carrier is a hard policer, not a fair
	// queue: with nc=0 the unavoidable ~4% drops collapsed cwnd and starved
	// the wire. With nc=1 KCP keeps the window full and retransmits the few
	// losses, letting throughput reach the SFU's real ceiling.
	sess.SetNoDelay(1, 5, 2, 1)
	sess.SetWindowSize(kcpSndWnd, kcpRcvWnd)
	sess.SetMtu(kcpMTU)
	// Upstream marked SetStreamMode deprecated without providing a replacement;
	// stream framing is still required for our wire format.
	sess.SetStreamMode(true) //nolint:staticcheck // SA1019: no replacement upstream.
	sess.SetACKNoDelay(true)
	sess.SetWriteDelay(false)

	rt := &kcpRuntime{
		conn:     c,
		sess:     sess,
		readDone: make(chan struct{}),
		stopCh:   make(chan struct{}),
	}
	rt.pacerCtx, rt.pacerCancel = context.WithCancel(context.Background())

	go rt.readLoop(onData)

	// Brutal pacing: install an outbound-layer token bucket at the target rate
	// and launch the loss compensator. The bucket paces VP8 samples as they
	// leave the outbound queue in writerLoop/peerWriterPump — KCP itself stays
	// unpaced and feels congestion through its own send-window back-pressure as
	// the outbound channel fills. brutalBps == 0 leaves limiter nil, so pace()
	// is a no-op and the legacy behaviour is byte-for-byte unchanged.
	if brutalBps > 0 {
		burst := brutalBps / 10
		if burst < brutalMinBurst {
			burst = brutalMinBurst
		}
		rt.limiter = rate.NewLimiter(rate.Limit(brutalBps), burst)
		go rt.brutalLoop(brutalBps)
	}

	return rt, nil
}

// pace blocks until the outbound token bucket has n bytes of allowance, so the
// VP8 sample about to be written respects the Hysteria "brutal" send rate. It
// is a no-op when pacing is disabled (limiter == nil). A cancelled pacerCtx
// (session closing) returns immediately without erroring the caller: the write
// path just proceeds, and close() tears the session down right after.
func (r *kcpRuntime) pace(n int) {
	if r.limiter == nil || n <= 0 {
		return
	}
	// WaitN only errors on ctx cancellation or n > burst; burst is floored at
	// brutalMinBurst (>= defaultMaxPayloadSize) so n never exceeds it. Either
	// way we fall through to the write — pacing is best-effort, not a gate.
	_ = r.limiter.WaitN(r.pacerCtx, n)
}

// brutalLoop implements Hysteria "brutal"-style loss compensation for one
// data-KCP session. Every brutalInterval it measures the incoming loss rate on
// this path (per-interval deltas of kcpConn.delivered/dropped) and raises the
// outbound pacer's rate limit to target/(1-loss) so goodput holds at the
// configured target despite drops.
//
// APPROXIMATION / LIMITATION: we measure INCOMING loss (packets this path fails
// to receive: CRC failure = carrier corruption, or inbound-queue overflow) yet
// compensate the OUTGOING send rate. kcp-go exposes no per-session outbound
// retransmit stats and there is no loss-report protocol from the remote
// receiver yet, so incoming loss is used as a SYMMETRIC PROXY for outgoing
// loss. The clamps in brutalEffectiveBps bound the resulting escalation.
//
// CONSEQUENCE FOR THE BULK SENDER: loss is measured on INBOUND packets but the
// compensated rate is applied OUTBOUND. The peer that is *sending* bulk data
// (e.g. the server during a download) receives almost nothing on its data-KCP
// but ACKs, so its delivered/dropped counters barely move and its measured loss
// stays ~0 — meaning loss compensation is effectively INACTIVE on the sender
// and it simply paces at the fixed target. That is acceptable for v1: a fixed
// target-rate pacing on the sender is the primary value. Real loss compensation
// only kicks in on the side that *receives* the bulk stream. A true bidirectional
// signal needs a loss-report over the control plane (future work).
func (r *kcpRuntime) brutalLoop(targetBps int) {
	ticker := time.NewTicker(brutalInterval)
	defer ticker.Stop()

	var prevDelivered, prevDropped uint64
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			delivered := r.conn.delivered.Load()
			dropped := r.conn.dropped.Load()
			winDelivered := delivered - prevDelivered
			winDropped := dropped - prevDropped
			prevDelivered, prevDropped = delivered, dropped

			// Empty window guard: with no delivered and no dropped packets the
			// ratio would be 0/0 = NaN. Keep lossRate at 0 in that case so we
			// pace at the target. brutalEffectiveBps is independently NaN-safe,
			// but computing NaN here at all is avoided so the intent is explicit.
			total := winDelivered + winDropped
			var lossRate float64
			if total > 0 {
				lossRate = float64(winDropped) / float64(total)
			}
			// eff is guaranteed in [targetBps, targetBps*brutalMaxRateMul] and
			// strictly > 0 for targetBps > 0, so the pacer is never given a
			// zero/non-finite rate (which would stall the send path).
			eff := brutalEffectiveBps(targetBps, lossRate)
			r.limiter.SetLimit(rate.Limit(eff))
		}
	}
}

func (r *kcpRuntime) readLoop(onData func([]byte)) {
	defer close(r.readDone)

	var hdr [kcpLenPrefix]byte
	for {
		if _, err := io.ReadFull(r.sess, hdr[:]); err != nil {
			return
		}
		size := binary.BigEndian.Uint32(hdr[:])
		if size == 0 {
			continue
		}
		if size > kcpMaxMessage {
			return
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(r.sess, payload); err != nil {
			return
		}
		if onData != nil {
			onData(payload)
		}
	}
}

// deliver hands a wire payload (already reassembled out of VP8 RTP) to KCP.
func (r *kcpRuntime) deliver(payload []byte) {
	r.conn.deliver(payload)
}

// setHeader re-points the outgoing frame header so subsequent KCP packets are
// addressed to a specific destination epoch (see kcpConn.setHeader).
func (r *kcpRuntime) setHeader(hdr [epochHdrLen]byte) {
	r.conn.setHeader(hdr)
}

// send queues an application message for reliable delivery. The length
// prefix + payload pair is written under a mutex so that interleaved
// concurrent senders cannot tear the framing.
func (r *kcpRuntime) send(msg []byte) error {
	if len(msg) > kcpMaxMessage {
		return ErrKCPMessageTooLarge
	}
	var hdr [kcpLenPrefix]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(msg))) //nolint:gosec,lll // G115: bounded conversion verified by surrounding logic

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	if _, err := r.sess.Write(hdr[:]); err != nil {
		return fmt.Errorf("kcp write header: %w", err)
	}
	if _, err := r.sess.Write(msg); err != nil {
		return fmt.Errorf("kcp write payload: %w", err)
	}
	return nil
}

func (r *kcpRuntime) close() {
	r.closeOnce.Do(func() {
		close(r.stopCh)
		if r.pacerCancel != nil {
			r.pacerCancel() // unblock any in-flight pace() WaitN
		}
		_ = r.sess.Close()
		_ = r.conn.Close()
	})
}
