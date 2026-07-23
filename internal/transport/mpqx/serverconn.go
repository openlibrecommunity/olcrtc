package mpqx

import (
	"net"
	"sync"
	"time"
)

// PeerSender is the outbound half of a peer-aware carrier: a single call ships
// one message to a specific remote peer identified by peerID. A server-side
// [transport.PeerTransport] satisfies this via its SendTo method.
type PeerSender interface {
	SendTo(peerID string, data []byte) error
}

// peerAddr is a stable, unique-per-peer net.Addr synthesised from a carrier
// peerID. mpq-brutal's server demux (apernet/quic-go) routes inbound QUIC
// packets to connections by the source net.Addr, so every distinct peer MUST
// map to a distinct, stable address — otherwise two clients' (or two paths')
// packets would collapse onto one QUIC connection. The value is the peerID
// verbatim, which is already unique and stable for the lifetime of a peer
// (a vp8channel epoch hex, a datachannel endpoint id, etc.).
type peerAddr string

func (a peerAddr) Network() string { return "mpqx-peer" }
func (a peerAddr) String() string  { return string(a) }

// peerPacket is one inbound carrier message tagged with its source address.
type peerPacket struct {
	data []byte
	addr net.Addr
}

// ServerPacketConn adapts a peer-aware carrier ([PeerSender] for outbound plus a
// per-peer OnPeerData inbound stream) into a single net.PacketConn that
// mpq-brutal's core.Listen consumes as its ServerConn. Several remote peers
// (distinct clients, or distinct paths of one client) share this one conn; each
// is given a stable synthetic net.Addr so quic-go can demux them into separate
// QUIC connections, which mpq then regroups into bonding sessions by sessionID.
//
// It is the server-side analogue of the memHub in mpq-brutal's inject_test: one
// ServerConn, per-source ReadFrom addresses, address-routed WriteTo. It is safe
// for concurrent use.
type ServerPacketConn struct {
	sender PeerSender

	inbox chan peerPacket

	mu sync.Mutex
	// peerToAddr / addrToPeer are the bidirectional peerID<->net.Addr map. A
	// peer's address is minted lazily on first Deliver and cached so every
	// packet from that peer yields the identical net.Addr value (quic-go keys
	// its connection map on the address).
	peerToAddr map[string]net.Addr
	addrToPeer map[string]string
	sender2    PeerSender // guarded copy set via Bind after construction
	deadline   time.Time  // read deadline; zero means none
	// dlChanged is closed on every SetReadDeadline so a blocked ReadFrom wakes
	// and recomputes its timeout (mirrors the 1:1 PacketConn / a real socket).
	dlChanged chan struct{}

	closeOnce sync.Once
	done      chan struct{}
}

var _ net.PacketConn = (*ServerPacketConn)(nil)

// NewServer creates a peer-aware server adapter. sender may be nil and supplied
// later via [ServerPacketConn.Bind] (the carrier often needs this adapter's
// Deliver as its OnPeerData before it can be constructed). Register the returned
// adapter's Deliver method as the carrier's OnPeerData callback.
func NewServer(sender PeerSender) *ServerPacketConn {
	return &ServerPacketConn{
		sender:     sender,
		inbox:      make(chan peerPacket, inboxCap),
		peerToAddr: make(map[string]net.Addr),
		addrToPeer: make(map[string]string),
		dlChanged:  make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// Bind sets (or replaces) the outbound peer sender used by WriteTo. Use it when
// the carrier could not be constructed until this adapter's Deliver existed.
func (c *ServerPacketConn) Bind(sender PeerSender) {
	c.mu.Lock()
	c.sender2 = sender
	c.mu.Unlock()
}

// currentSender returns the effective peer sender (a Bind override wins).
func (c *ServerPacketConn) currentSender() PeerSender {
	c.mu.Lock()
	s := c.sender2
	c.mu.Unlock()
	if s != nil {
		return s
	}
	return c.sender
}

// addrForPeer returns the stable synthetic address for peerID, minting and
// caching it on first use.
func (c *ServerPacketConn) addrForPeer(peerID string) net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a, ok := c.peerToAddr[peerID]; ok {
		return a
	}
	a := peerAddr(peerID)
	c.peerToAddr[peerID] = a
	c.addrToPeer[a.String()] = peerID
	return a
}

// Deliver feeds one inbound carrier message from a specific peer to the adapter.
// Register it as the carrier's OnPeerData callback. It copies p (the carrier may
// reuse its buffer), tags it with the peer's stable address and enqueues it for
// ReadFrom. If the queue is full or the adapter is closed the message is dropped
// — QUIC recovers dropped packets by retransmission.
func (c *ServerPacketConn) Deliver(peerID string, p []byte) {
	addr := c.addrForPeer(peerID)
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case c.inbox <- peerPacket{data: cp, addr: addr}:
	case <-c.done:
	default:
		// Inbox full: drop, like a saturated datagram socket.
	}
}

// ReadFrom returns the next inbound message and the synthetic address of the
// peer it came from. It blocks until data arrives, the read deadline fires, or
// the adapter is closed.
func (c *ServerPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
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
		case pkt := <-c.inbox:
			if timer != nil {
				timer.Stop()
			}
			n := copy(p, pkt.data)
			return n, pkt.addr, nil
		case <-timeoutCh:
			return 0, nil, timeoutError{}
		case <-changed:
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

// WriteTo ships p to the peer whose synthetic address is addr, via the carrier's
// SendTo. An address with no known peer (never seen inbound) is dropped exactly
// as a datagram to an unknown destination would be — QUIC retransmits if needed.
// It copies p before handing it to the carrier so the caller may reuse it.
func (c *ServerPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}
	c.mu.Lock()
	peerID, ok := c.addrToPeer[addr.String()]
	c.mu.Unlock()
	if !ok {
		// Unknown peer: behave like a datagram sent into the void.
		return len(p), nil
	}
	s := c.currentSender()
	if s == nil {
		return 0, net.ErrClosed
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	if err := s.SendTo(peerID, cp); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close is idempotent. It unblocks any pending ReadFrom and makes further
// WriteTo fail. It does NOT close the underlying carrier (see package doc).
func (c *ServerPacketConn) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	return nil
}

// LocalAddr returns the fixed synthetic local address.
func (c *ServerPacketConn) LocalAddr() net.Addr { return localAddr }

// SetDeadline sets the read deadline (the write side never blocks).
func (c *ServerPacketConn) SetDeadline(t time.Time) error {
	return c.SetReadDeadline(t)
}

// SetReadDeadline sets the deadline for future ReadFrom calls and wakes any
// ReadFrom currently blocked so it observes the new deadline. Required: quic-go
// stops the read loop of a borrowed conn by setting a read deadline.
func (c *ServerPacketConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	old := c.dlChanged
	c.dlChanged = make(chan struct{})
	c.mu.Unlock()
	close(old)
	return nil
}

// SetWriteDeadline is a no-op: WriteTo hands off to the carrier synchronously.
func (c *ServerPacketConn) SetWriteDeadline(time.Time) error { return nil }
