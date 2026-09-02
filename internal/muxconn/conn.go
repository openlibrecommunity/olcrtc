// Package muxconn adapts a link.Link into an io.ReadWriteCloser suitable for
// driving a smux session. The wrapper applies AEAD on every wire-bound write
// and inverts it on every received message before exposing the bytes as a
// byte stream.
//
// Link semantics are message-oriented: each Send produces exactly one OnData
// on the peer. smux operates on a pure byte stream (header + payload may be
// glued or split across reads). We bridge by:
//
//   - Treating each Push as an opaque chunk handed off via a channel that
//     Read drains in arbitrary slices, retaining any tail bytes that did
//     not fit the caller's buffer for the next Read.
//   - Letting smux's sendLoop call Write once per frame; we encrypt and hand
//     the whole buffer to the link as a single message. Length boundaries
//     are preserved end-to-end by the transport (KCP length-prefix framing
//     in vp8channel, native message boundaries in datachannel).
package muxconn

import (
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
)

// ErrClosed is returned from Read/Write after the conn has been closed.
var ErrClosed = errors.New("muxconn: closed")

// ErrWriteTimeout is returned from Write when the transport never reports
// CanSend within writeReadyTimeout. It satisfies net.Error with Timeout()
// true, which is how smux (and the net package contract) recognises a
// deadline rather than a protocol fault.
var ErrWriteTimeout net.Error = writeTimeoutError{}

type writeTimeoutError struct{}

func (writeTimeoutError) Error() string { return "muxconn: write timeout waiting for transport" }

func (writeTimeoutError) Timeout() bool { return true }

func (writeTimeoutError) Temporary() bool { return true }

// errReadClosed is what Read reports once the conn is closed and drained.
//
// smux stores whatever error the underlying conn returns and hands it to
// stream readers verbatim; its recvLoop never compares against io.EOF, so
// widening the error is safe there. We still unwrap to io.EOF because
// every io.Reader consumer (and the io contract) treats EOF as the clean
// end of a stream, while errors.Is(err, ErrClosed) now lets callers tell a
// deliberate close from an aborted link.
var errReadClosed error = closedReadError{}

type closedReadError struct{}

func (closedReadError) Error() string { return "muxconn: closed (EOF)" }

func (closedReadError) Unwrap() []error { return []error{io.EOF, ErrClosed} }

const (
	dataRecordAAD    = "olcrtc/muxconn/v2/data"
	controlRecordAAD = "olcrtc/muxconn/v2/control"

	// inboundQueue is the buffered capacity of the Push -> Read pipeline.
	// It absorbs short Read stalls without applying back-pressure to the
	// transport callback. Frames are typically smux-sized (up to 32 KiB),
	// so 128 amounts to a few MiB of in-flight data, which is
	// enough for sustained throughput without letting a stuck reader retain
	// a large pool-backed working set per connection.
	inboundQueue = 128

	// pooledFrameCap is the capacity each pooled plaintext buffer is born
	// with. It is sized to fit the largest smux frame any of our
	// transports will deliver after AEAD overhead is stripped (datachannel
	// caps at 12 KiB on the wire, vp8channel at 60 KiB; we round up to
	// give Open room to write in place without growing the slice).
	pooledFrameCap = 64 * 1024

	// writeReadyTimeout bounds how long Write waits for the transport to
	// accept data. A transport that cannot send for this long is dead in
	// every practical sense - it exceeds the smux keepalive timeout - so
	// failing the write (and with it the session) beats parking the smux
	// send loop for the lifetime of the process.
	writeReadyTimeout = 30 * time.Second

	// decryptLogInterval bounds how often a conn reports frames that failed
	// to decrypt. Any participant in the same SFU room can spray junk into
	// our channel, so the steady state must not be one log line per frame.
	decryptLogInterval = 30 * time.Second
)

// frameBufPool recycles plaintext buffers between Push (decrypts a wire
// frame into a buffer) and Read (consumes the buffer fully then returns
// it). It is global so all Conn instances share the same hot cache -
// most clients in the same process talk to a handful of peers, and
// per-Conn pools fragment the warm set unnecessarily.
var frameBufPool = sync.Pool{ //nolint:gochecknoglobals // intentional process-wide buffer pool
	New: func() any {
		b := make([]byte, 0, pooledFrameCap)
		return &b
	},
}

func acquireFrameBuf() *[]byte {
	bp, ok := frameBufPool.Get().(*[]byte)
	if !ok {
		b := make([]byte, 0, pooledFrameCap)
		return &b
	}
	*bp = (*bp)[:0]
	return bp
}

func releaseFrameBuf(bp *[]byte) {
	if bp == nil {
		return
	}
	// Drop oversized buffers so a one-off huge frame can't poison the
	// pool's working set forever.
	if cap(*bp) > pooledFrameCap*2 {
		return
	}
	*bp = (*bp)[:0]
	frameBufPool.Put(bp)
}

// Conn is an io.ReadWriteCloser over a [transport.Transport] with v2 record protection.
//
// Push produces decrypted plaintext frames into an internal channel; Read
// drains the channel and slices each frame across as many caller buffers
// as needed. The hot path is lock-free: a single producer (the transport
// callback) and a single consumer (smux's read loop) communicate via a
// buffered channel without any cond/mutex ping-pong.
//
// Plaintext buffers are recycled through frameBufPool: Push borrows a
// buffer to decrypt into, ships it through the channel, and Read returns
// the buffer to the pool once its caller has consumed all the bytes.
type Conn struct {
	ln      transport.Transport
	send    func([]byte) error
	canSend func() bool // if nil, uses ln.CanSend
	keys    interface {
		SealInto([]byte, []byte, []byte) ([]byte, error)
		OpenInto([]byte, []byte, []byte) ([]byte, error)
	}
	aad []byte

	in        chan *[]byte
	closeOnce sync.Once
	closeCh   chan struct{}
	closed    atomic.Bool

	// leftoverBuf holds the pool buffer whose tail is still in
	// `leftover`. When `leftover` empties we return leftoverBuf to the
	// pool and clear both fields. Touched only by Read.
	leftoverBuf *[]byte
	leftover    []byte

	// decrypt failure accounting for the rate-limited log in Push.
	decryptFails  atomic.Uint64
	decryptLogged atomic.Uint64
	decryptLogAt  atomic.Int64

	// writeTimeout overrides writeReadyTimeout. Zero means the default;
	// only tests set it.
	writeTimeout time.Duration
}

func (c *Conn) sendDeadline() time.Duration {
	if c.writeTimeout > 0 {
		return c.writeTimeout
	}
	return writeReadyTimeout
}

// New wires a Conn over the given transport. Push must be set as the
// transport's OnData callback before this conn is used.
func New(ln transport.Transport, keys interface {
	SealInto([]byte, []byte, []byte) ([]byte, error)
	OpenInto([]byte, []byte, []byte) ([]byte, error)
}) *Conn {
	return &Conn{
		ln:      ln,
		send:    ln.Send,
		keys:    keys,
		aad:     []byte(dataRecordAAD),
		in:      make(chan *[]byte, inboundQueue),
		closeCh: make(chan struct{}),
	}
}

// NewControl wires a Conn that routes through the transport's isolated
// control-plane channel (transport.ControlPlane). Returns nil if the
// transport does not implement ControlPlane.
func NewControl(ln transport.Transport, keys interface {
	SealInto([]byte, []byte, []byte) ([]byte, error)
	OpenInto([]byte, []byte, []byte) ([]byte, error)
}) *Conn {
	cp, ok := ln.(transport.ControlPlane)
	if !ok {
		return nil
	}
	c := &Conn{
		ln:      ln,
		send:    cp.ControlSend,
		canSend: cp.ControlCanSend,
		keys:    keys,
		aad:     []byte(controlRecordAAD),
		in:      make(chan *[]byte, inboundQueue),
		closeCh: make(chan struct{}),
	}
	cp.SetControlOnData(func(data []byte) { c.Push(data) })
	return c
}

// NewPeer wires a Conn whose writes are addressed to a specific transport peer.
func NewPeer(ln transport.PeerTransport, keys interface {
	SealInto([]byte, []byte, []byte) ([]byte, error)
	OpenInto([]byte, []byte, []byte) ([]byte, error)
}, peerID string) *Conn {
	return &Conn{
		ln: ln,
		send: func(data []byte) error {
			return ln.SendTo(peerID, data)
		},
		keys:    keys,
		aad:     []byte(dataRecordAAD),
		in:      make(chan *[]byte, inboundQueue),
		closeCh: make(chan struct{}),
	}
}

// NewPeerControlUnbound wires a Conn to the per-peer control plane of a
// transport.PeerControlPlane. Returns nil if the transport does not implement
// PeerControlPlane.
//
// Unlike NewControl, the returned conn is NOT bound to the transport: the
// caller must feed it via Push. PeerControlPlane exposes a single
// SetControlOnPeerData callback covering every peer, so registering here
// would clobber the caller's demultiplexer - hence the name.
func NewPeerControlUnbound(ln transport.Transport, keys interface {
	SealInto([]byte, []byte, []byte) ([]byte, error)
	OpenInto([]byte, []byte, []byte) ([]byte, error)
}, peerID string) *Conn {
	cp, ok := ln.(transport.PeerControlPlane)
	if !ok {
		return nil
	}
	c := &Conn{
		ln: ln,
		send: func(data []byte) error {
			return cp.ControlSendTo(peerID, data)
		},
		canSend: func() bool {
			return cp.ControlPeerCanSend(peerID)
		},
		keys:    keys,
		aad:     []byte(controlRecordAAD),
		in:      make(chan *[]byte, inboundQueue),
		closeCh: make(chan struct{}),
	}
	return c
}

// Push hands an encrypted wire payload (one OnData event) to the conn.
//
// On the producer side: borrow a pooled plaintext buffer, decrypt into
// it, then either deliver via the inbound channel or, if the caller has
// Close'd, return the buffer to the pool. Blocking forever on a wedged
// reader would wedge the transport callback and trip its watchdog, so we
// also bail on closeCh.
func (c *Conn) Push(ciphertext []byte) {
	bufPtr := acquireFrameBuf()
	pt, err := c.keys.OpenInto(*bufPtr, ciphertext, c.aad)
	if err != nil {
		releaseFrameBuf(bufPtr)
		c.noteDecryptFailure(len(ciphertext), err)
		return
	}
	*bufPtr = pt
	if c.closed.Load() {
		releaseFrameBuf(bufPtr)
		return
	}
	select {
	case c.in <- bufPtr:
	case <-c.closeCh:
		releaseFrameBuf(bufPtr)
	}
}

// noteDecryptFailure records an undecryptable frame and logs the first one
// plus a periodic summary carrying how many were dropped since the last
// report. Frames from unrelated room participants are expected traffic, not
// an error worth one line each.
func (c *Conn) noteDecryptFailure(size int, err error) {
	total := c.decryptFails.Add(1)
	now := time.Now().UnixNano()
	if total == 1 {
		c.decryptLogAt.Store(now)
		c.decryptLogged.Store(total)
		logger.Warnf("muxconn: decrypt failed len=%d: %v", size, err)
		return
	}
	last := c.decryptLogAt.Load()
	if now-last < int64(decryptLogInterval) {
		return
	}
	if !c.decryptLogAt.CompareAndSwap(last, now) {
		return
	}
	dropped := total - c.decryptLogged.Swap(total)
	logger.Warnf("muxconn: decrypt failed for %d more frames in the last %s, latest len=%d: %v",
		dropped, decryptLogInterval, size, err)
}

// Read implements io.Reader. Blocks until at least one byte is available;
// after that, drains additional ready frames non-blockingly to fill p, so
// a single Read can absorb several queued frames in one go. This matches
// the prior cond/append-based implementation's concatenation behaviour
// and lets smux's bufio reader pull large chunks at a time.
func (c *Conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(c.leftover) == 0 {
		bufPtr, ok := c.takeFrame()
		if !ok {
			return 0, errReadClosed
		}
		c.leftoverBuf = bufPtr
		c.leftover = *bufPtr
	}
	n := copy(p, c.leftover)
	c.leftover = c.leftover[n:]
	c.recycleIfDrained()

	// Greedily pull additional frames already sitting in the queue,
	// without blocking. This keeps the channel from accumulating a
	// backlog when the consumer asks for a large buffer.
	for n < len(p) && len(c.leftover) == 0 {
		select {
		case bufPtr, ok := <-c.in:
			if !ok {
				return n, nil
			}
			data := *bufPtr
			m := copy(p[n:], data)
			n += m
			if m < len(data) {
				c.leftoverBuf = bufPtr
				c.leftover = data[m:]
			} else {
				releaseFrameBuf(bufPtr)
			}
		default:
			return n, nil
		}
	}
	return n, nil
}

// takeFrame blocks until a frame is available or the conn is closed.
// On close it still picks up a frame that raced past Close's drain, so a
// peer that shuts us down right after a final write doesn't lose data.
func (c *Conn) takeFrame() (*[]byte, bool) {
	select {
	case bufPtr, ok := <-c.in:
		if !ok {
			return nil, false
		}
		return bufPtr, true
	case <-c.closeCh:
		select {
		case bufPtr, ok := <-c.in:
			if !ok {
				return nil, false
			}
			return bufPtr, true
		default:
			return nil, false
		}
	}
}

func (c *Conn) recycleIfDrained() {
	if len(c.leftover) == 0 && c.leftoverBuf != nil {
		releaseFrameBuf(c.leftoverBuf)
		c.leftoverBuf = nil
	}
}

// Write encrypts p and ships it to the link as a single message. Blocks while
// the link signals back-pressure, up to writeReadyTimeout.
func (c *Conn) Write(p []byte) (int, error) {
	if err := c.waitSendReady(); err != nil {
		return 0, err
	}

	enc, err := c.keys.SealInto(nil, p, c.aad)
	if err != nil {
		return 0, fmt.Errorf("encrypt: %w", err)
	}
	if err := c.send(enc); err != nil {
		return 0, fmt.Errorf("send: %w", err)
	}
	return len(p), nil
}

// waitSendReady blocks until the link accepts data, the conn closes, or
// writeReadyTimeout elapses.
//
// A short spin comes first: on a healthy link CanSend usually clears well
// under a millisecond, and sleeping straight away adds visible per-frame
// latency to interactive request/response traffic. Only a genuinely
// congested link falls through to the polling loop.
func (c *Conn) waitSendReady() error {
	const (
		fastSpinAttempts = 16
		slowPollDelay    = 2 * time.Millisecond
	)
	canSend := c.canSend
	if canSend == nil {
		canSend = c.ln.CanSend
	}
	for range fastSpinAttempts {
		if c.closed.Load() {
			return ErrClosed
		}
		if canSend() {
			return nil
		}
		runtime.Gosched()
	}

	deadline := time.NewTimer(c.sendDeadline())
	defer deadline.Stop()
	ticker := time.NewTicker(slowPollDelay)
	defer ticker.Stop()
	for {
		if c.closed.Load() {
			return ErrClosed
		}
		if canSend() {
			return nil
		}
		select {
		case <-c.closeCh:
			return ErrClosed
		case <-deadline.C:
			return ErrWriteTimeout
		case <-ticker.C:
		}
	}
}

// Close unblocks any pending Read with errReadClosed and returns every
// queued inbound buffer to the pool. Frames still sitting in the queue are
// dropped: the caller asked to tear the conn down, and holding pooled
// buffers alive past that only costs the pool its warm working set.
func (c *Conn) Close() error { //nolint:unparam // io.Closer requires an error result
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.closeCh)
		c.drainInbound()
	})
	return nil
}

func (c *Conn) drainInbound() {
	for {
		select {
		case bufPtr := <-c.in:
			releaseFrameBuf(bufPtr)
		default:
			return
		}
	}
}
