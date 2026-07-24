// Package mpqx adapts a message-oriented [transport.Transport] carrier into a
// net.PacketConn so that it can carry a bonding QUIC session from
// github.com/SolverNA/mpq-brutal.
//
// mpq-brutal (and the underlying apernet/quic-go) speaks over any
// net.PacketConn: one QUIC packet maps to one carrier message. The client side
// plugs a [PacketConn] in through core.Config.DialPath; the server side plugs
// one in through core.Config.ServerConn. This package provides that glue for a
// single 1:1 carrier link.
//
// # Wiring
//
// The carrier delivers inbound bytes through the OnData callback supplied at
// construction time (transport.Config.OnData), while outbound bytes go through
// the carrier's Send method. Because the carrier needs this adapter's inbound
// sink at the moment it is built, the usual order is:
//
//	pc := mpqx.New(nil)                       // inbox ready, no sender yet
//	tr, _ := datachannel.New(ctx, transport.Config{OnData: pc.Deliver, ...})
//	pc.Bind(tr)                                // now WriteTo can Send
//
// or, when the carrier already exists, simply mpqx.New(tr) and register
// pc.Deliver as the carrier's OnData.
//
// # Ownership
//
// The adapter does NOT own the carrier: Close only unblocks pending ReadFrom
// calls and makes further WriteTo fail; it never closes the underlying
// transport. The caller that created the carrier remains responsible for
// closing it.
package mpqx

import (
	"net"
	"sync"
	"time"
)

// MaxQUICPacketSize is the recommended maximum size of a single QUIC packet
// (and therefore of a single carrier message) when running mpq-brutal over a
// carrier. quic-go should be configured with DisablePathMTUDiscovery and an
// initial packet size no larger than this so that no QUIC packet ever exceeds
// the carrier's own MaxPayloadSize. 1200 bytes is the conservative IPv6
// minimum-MTU-safe value quic-go uses by default.
const MaxQUICPacketSize = 1200

// PathMaxIdleTimeout and PathKeepAlivePeriod tune how fast mpq detects a dead
// path (a carrier/room that has silently gone away) when running over an
// unreliable carrier. QUIC declares a path dead once no packet arrives for
// MaxIdleTimeout; over UDP mpq-brutal defaults to ~45s, which on WebRTC would
// mean up to ~45s of degraded service after a room dies. Here we shrink the
// idle timeout to a few seconds and send keep-alive PINGs well inside it: while
// the carrier is alive the PINGs keep an idle path from dying, but once the
// carrier is truly dead the PINGs go unacked, no packets arrive, and the idle
// timer fires within PathMaxIdleTimeout — so a dead path is evicted from the mpq
// scheduler in a handful of seconds instead of ~45s. Both endpoints must agree
// on a small idle timeout (QUIC negotiates the min), so client and server pass
// these same values.
const (
	PathMaxIdleTimeout  = 6 * time.Second
	PathKeepAlivePeriod = 2 * time.Second
)

// Sender is the outbound half of a carrier: a single call ships one message.
// [transport.Transport] satisfies this interface via its Send method.
type Sender interface {
	Send(data []byte) error
}

// syntheticAddr is a fixed, stable net.Addr used for both endpoints of the
// adapter. mpq-brutal only needs a stable, unique-per-peer address for its
// demux; for a 1:1 link one constant address on each side is sufficient.
// apernet/quic-go accepts an arbitrary net.Addr (it does not require a
// *net.UDPAddr) — proven by mpq-brutal's inject_test.
type syntheticAddr string

func (a syntheticAddr) Network() string { return "mpqx" }
func (a syntheticAddr) String() string  { return string(a) }

// LocalAddr / remote addresses of the adapter. A single constant per role is
// enough for a 1:1 carrier; quic-go tolerates any net.Addr here.
const (
	localAddr  syntheticAddr = "mpqx-local"
	remoteAddr syntheticAddr = "mpqx-remote"
)

// RemoteAddr returns the synthetic remote address that a client-side DialPath
// factory should hand back to mpq-brutal alongside the [PacketConn].
func RemoteAddr() net.Addr { return remoteAddr }

// inboxCap bounds the number of buffered inbound messages. At ~1200 bytes each
// this is a bit over 1 MB of slack, enough to absorb bursts while the QUIC read
// loop drains the queue. On overflow the newest packet is dropped, exactly as a
// real datagram socket would drop under pressure — QUIC's loss recovery
// retransmits, so correctness is preserved.
const inboxCap = 1024

// PacketConn adapts a carrier [Sender] plus its inbound OnData stream into a
// net.PacketConn. It is safe for concurrent use: WriteTo, ReadFrom, Deliver,
// the SetDeadline family, and Close may all run from different goroutines.
type PacketConn struct {
	sender Sender

	inbox chan []byte

	mu       sync.Mutex
	sender2  Sender    // guarded copy set via Bind after construction
	deadline time.Time // read deadline; zero means none
	// dlChanged is closed on every SetReadDeadline so a blocked ReadFrom wakes
	// and recomputes its timeout. This mirrors a real socket, where
	// SetReadDeadline(now) interrupts an in-flight Read — the mechanism quic-go
	// relies on to stop the read loop of a borrowed conn during Transport.Close.
	dlChanged chan struct{}

	closeOnce sync.Once
	done      chan struct{}
}

var _ net.PacketConn = (*PacketConn)(nil)

// New creates an adapter. sender may be nil and supplied later via [Bind] (the
// carrier often needs this adapter's Deliver as its OnData before it can be
// constructed). Register the returned adapter's Deliver method as the carrier's
// inbound (OnData) callback.
func New(sender Sender) *PacketConn {
	return &PacketConn{
		sender:    sender,
		inbox:     make(chan []byte, inboxCap),
		dlChanged: make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Bind sets (or replaces) the outbound sender used by WriteTo. Use it when the
// carrier could not be constructed until this adapter's Deliver existed.
func (c *PacketConn) Bind(sender Sender) {
	c.mu.Lock()
	c.sender2 = sender
	c.mu.Unlock()
}

// currentSender returns the effective sender (a Bind override wins over the
// constructor value).
func (c *PacketConn) currentSender() Sender {
	c.mu.Lock()
	s := c.sender2
	c.mu.Unlock()
	if s != nil {
		return s
	}
	return c.sender
}

// Deliver feeds one inbound carrier message to the adapter. Register it as the
// carrier's OnData callback. It copies p (the carrier may reuse its buffer) and
// enqueues it for ReadFrom. If the queue is full or the adapter is closed the
// message is dropped — QUIC recovers dropped packets by retransmission.
func (c *PacketConn) Deliver(p []byte) {
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case c.inbox <- cp:
	case <-c.done:
	default:
		// Inbox full: drop, like a saturated datagram socket.
	}
}

// ReadFrom returns the next inbound message. It blocks until data arrives, the
// read deadline fires, or the adapter is closed. The returned net.Addr is the
// fixed synthetic remote address.
func (c *PacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		c.mu.Lock()
		dl := c.deadline
		changed := c.dlChanged
		c.mu.Unlock()

		var timeoutCh <-chan time.Time
		var timer *time.Timer
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return 0, nil, timeoutError{}
			}
			timer = time.NewTimer(d)
			timeoutCh = timer.C
		}

		select {
		case msg := <-c.inbox:
			if timer != nil {
				timer.Stop()
			}
			n := copy(p, msg)
			return n, remoteAddr, nil
		case <-timeoutCh:
			return 0, nil, timeoutError{}
		case <-changed:
			// Deadline was updated: recompute and re-enter the select.
			if timer != nil {
				timer.Stop()
			}
			continue
		case <-c.done:
			if timer != nil {
				timer.Stop()
			}
			return 0, nil, net.ErrClosed
		}
	}
}

// WriteTo ships p as a single carrier message. addr is ignored (the carrier is
// a 1:1 link). It copies p before handing it to the carrier so the caller may
// reuse the buffer immediately.
func (c *PacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}
	s := c.currentSender()
	if s == nil {
		return 0, net.ErrClosed
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	if err := s.Send(cp); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close is idempotent. It unblocks any pending ReadFrom and makes further
// WriteTo fail. It does NOT close the underlying carrier (see package doc).
func (c *PacketConn) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	return nil
}

// LocalAddr returns the fixed synthetic local address.
func (c *PacketConn) LocalAddr() net.Addr { return localAddr }

// SetDeadline sets the read deadline (the write side never blocks, so a write
// deadline is a no-op).
func (c *PacketConn) SetDeadline(t time.Time) error {
	return c.SetReadDeadline(t)
}

// SetReadDeadline sets the deadline for future ReadFrom calls and wakes any
// ReadFrom currently blocked so it observes the new deadline. A zero t clears
// the deadline. This is required: quic-go stops the read loop of a borrowed
// conn by setting a read deadline, so Close would hang without it.
func (c *PacketConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	old := c.dlChanged
	c.dlChanged = make(chan struct{})
	c.mu.Unlock()
	close(old) // wake a blocked ReadFrom so it re-reads the deadline
	return nil
}

// SetWriteDeadline is a no-op: WriteTo hands off to the carrier synchronously
// and never blocks on a deadline.
func (c *PacketConn) SetWriteDeadline(time.Time) error { return nil }

// timeoutError is a net.Error whose Timeout reports true, so quic-go recognises
// a read-deadline expiry (as opposed to a hard error).
type timeoutError struct{}

func (timeoutError) Error() string   { return "mpqx: read deadline exceeded" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
