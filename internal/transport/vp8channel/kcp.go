package vp8channel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	kcp "github.com/xtaci/kcp-go/v5"
)

// Both peers establish a KCP session with the same convid. KCP does not
// require a handshake - packets are matched by conv field, so a static
// constant gives us a symmetrical P2P setup.
const kcpConvID = 0xC0FFEE01

// KCP tuning targets a lossy, bursty provider (VP8 over an SFU). The defaults
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

// ErrKCPMessageTooLarge is returned by send when the message exceeds
// kcpMaxMessage.
var ErrKCPMessageTooLarge = errors.New("vp8channel: kcp message exceeds maximum size")

// kcpRuntime owns the KCP session and the goroutine that pumps reassembled
// messages from KCP up to cfg.OnData.
type kcpRuntime struct {
	conn      *kcpConn
	sess      *kcp.UDPSession
	readDone  chan struct{}
	writeMu   sync.Mutex // serializes framed writes; also guards writeBuf
	writeBuf  []byte     // ai-generated: framing scratch, reused under writeMu
	closeOnce sync.Once
}

func startKCP(out chan<- *packetBuffer, onData func([]byte), epochHdr [epochHdrLen]byte) (*kcpRuntime, error) {
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
	// congestion control because the provider is a hard policer, not a fair
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
	}

	go rt.readLoop(onData)

	return rt, nil
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

// send queues an application message for reliable delivery. Length prefix and
// payload go out in one Write: two writes under a mutex stop concurrent senders
// interleaving, but do not make the pair atomic against failure. If the header
// lands and the payload does not, the stream carries a prefix for bytes that
// were never sent and the reader cannot resynchronise.
//
// ai-generated: replaced the two Write calls with a single framed write into a
// reused buffer; added writeBuf and the cap check.
func (r *kcpRuntime) send(msg []byte) error {
	if len(msg) > kcpMaxMessage {
		return ErrKCPMessageTooLarge
	}

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	need := kcpLenPrefix + len(msg)
	if cap(r.writeBuf) < need {
		r.writeBuf = make([]byte, need)
	}
	buf := r.writeBuf[:need]
	binary.BigEndian.PutUint32(buf[:kcpLenPrefix], uint32(len(msg))) //nolint:gosec,lll // G115: bounded conversion verified by surrounding logic
	copy(buf[kcpLenPrefix:], msg)

	if _, err := r.sess.Write(buf); err != nil {
		return fmt.Errorf("kcp write framed message: %w", err)
	}
	return nil
}

func (r *kcpRuntime) close() {
	r.closeOnce.Do(func() {
		_ = r.sess.Close()
		_ = r.conn.Close()
	})
}

// kcpPlane owns one KCP session together with the outbound queue that carries
// its packets. The transport runs two of them - bulk data and control - with
// identical lifecycle rules, so start/restart/drain/close live here once
// instead of being written twice with only the field names changed.
type kcpPlane struct {
	out    chan *packetBuffer
	onData func([]byte)

	// lifecycleMu serializes start/restart/close. Without it two concurrent
	// restarts - a provider reconnect and an upper-layer ResetPeer fire
	// together during recovery - both build a runtime and the loser is
	// overwritten in place, stranding its goroutines and KCP windows for the
	// lifetime of the process. mu stays narrow: it only guards the pointer
	// read on the send hot path.
	lifecycleMu sync.Mutex
	closed      bool

	mu   sync.RWMutex
	rt   *kcpRuntime
	once sync.Once
}

func newKCPPlane(queueSize int, onData func([]byte)) *kcpPlane {
	return &kcpPlane{out: make(chan *packetBuffer, queueSize), onData: onData}
}

// get returns the live runtime, or nil when the plane has not started (or is
// mid-restart).
func (p *kcpPlane) get() *kcpRuntime {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.rt
}

// set installs rt directly. Used by tests to attach a hand-built runtime.
func (p *kcpPlane) set(rt *kcpRuntime) {
	p.mu.Lock()
	p.rt = rt
	p.mu.Unlock()
}

// start brings the plane up exactly once however many times Connect runs. The
// first result reports whether this call was the one that started it.
func (p *kcpPlane) start(hdr [epochHdrLen]byte) (bool, error) {
	var (
		started bool
		err     error
	)

	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	p.once.Do(func() {
		if p.closed {
			return
		}
		var rt *kcpRuntime
		rt, err = startKCP(p.out, p.onData, hdr)
		if err != nil {
			return
		}
		p.set(rt)
		started = true
	})

	return started, err
}

// restart drops queued packets and replaces the KCP state machine with a
// fresh one stamped with hdr.
func (p *kcpPlane) restart(hdr [epochHdrLen]byte) {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	if p.closed {
		return
	}

	p.drain()

	p.mu.Lock()
	old := p.rt
	p.rt = nil
	p.mu.Unlock()

	if old != nil {
		old.close()
	}

	rt, err := startKCP(p.out, p.onData, hdr)
	if err != nil {
		return
	}
	p.set(rt)
}

// drain discards everything still queued for the paced writer.
func (p *kcpPlane) drain() {
	for {
		select {
		case packet := <-p.out:
			packet.release()
		default:
			return
		}
	}
}

func (p *kcpPlane) close() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	p.closed = true

	if rt := p.get(); rt != nil {
		rt.close()
	}
}
