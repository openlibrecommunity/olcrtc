package mpqx

import (
	"net"
	"strconv"
	"sync"
	"time"
)

// ai-generated: new interface, phase 1 multi-room mpq (N carriers = N rooms).
// RoomSender is the outbound half of one room: a single call ships one message.
// For a peer-routing carrier it is a SendTo(peerID) wrapper; for a 1:1 carrier
// it is a Send wrapper that ignores peerID.
type RoomSender interface {
	Send(peerID string, p []byte) error
}

// ai-generated: new type, phase 1 multi-room mpq. roomKey names one remote
// endpoint of the multi-room conn: the room it lives in plus its peer identity
// ("" for a 1:1 room).
type roomKey struct {
	roomID string
	peerID string
}

// ai-generated: new type, phase 1 multi-room mpq. roomPacket is one inbound
// carrier message tagged with its source address.
type roomPacket struct {
	data []byte
	addr net.Addr
}

// ai-generated: new type, phase 1 multi-room mpq. roomAddr is a synthetic
// net.Addr for one (roomID, peerID) pair. The value is a length-prefixed
// encoding of both parts, so any two distinct pairs yield distinct strings no
// matter what bytes the parts contain (peerIDs here are vp8channel epoch hexs,
// but room URLs may hold almost anything). quic-go keys its connection map on
// the returned address, so distinct pairs demux into distinct QUIC connections.
type roomAddr string

func (a roomAddr) Network() string { return "mpqx-room" }
func (a roomAddr) String() string  { return string(a) }

// ai-generated: new function, phase 1 multi-room mpq. roomAddrFor mints the
// address of one (roomID, peerID) pair.
func roomAddrFor(roomID, peerID string) net.Addr {
	return roomAddr(strconv.Itoa(len(roomID)) + ":" + roomID + strconv.Itoa(len(peerID)) + ":" + peerID)
}

// ai-generated: new type (whole RoomConn implementation below), phase 1
// multi-room mpq. RoomConn adapts N room carriers into ONE server-side
// net.PacketConn so that a client's N paths spread across N rooms bond into a
// single mpq session while multiple clients keep working. It is the multi-room
// analogue of [ServerPacketConn]: the server brings up one carrier per
// configured room and routes everything through this shared conn, with each
// (room, peer) pair given a stable synthetic net.Addr so quic-go demuxes paths
// and clients into distinct QUIC connections, which mpq regroups by sessionID.
//
// It is safe for concurrent use: WriteTo, ReadFrom, Deliver, AddRoom, the
// SetDeadline family, and Close may all run from different goroutines.
type RoomConn struct {
	// rooms holds the outbound sender of every registered room.
	rooms map[string]RoomSender

	inbox chan roomPacket

	mu sync.Mutex
	// keyToAddr / addrToKey are the bidirectional (room,peer) -> addr maps. An
	// address is minted lazily on first Deliver and cached so every packet from
	// that pair yields the identical net.Addr value.
	keyToAddr map[roomKey]net.Addr
	addrToKey map[string]roomKey
	deadline  time.Time // read deadline; zero means none
	// dlChanged is closed on every SetReadDeadline so a blocked ReadFrom wakes
	// and recomputes its timeout (mirrors the 1:1 PacketConn / a real socket).
	dlChanged chan struct{}

	closeOnce sync.Once
	done      chan struct{}
}

var _ net.PacketConn = (*RoomConn)(nil)

// ai-generated: new method, phase 1 multi-room mpq. NewRoom creates an empty
// multi-room adapter. Register each room's outbound sender via
// [RoomConn.AddRoom] and each room carrier's inbound via [RoomConn.Deliver] (as
// the carrier's OnPeerData, or OnData with peerID "").
func NewRoom() *RoomConn {
	return &RoomConn{
		rooms:     make(map[string]RoomSender),
		inbox:     make(chan roomPacket, inboxCap),
		keyToAddr: make(map[roomKey]net.Addr),
		addrToKey: make(map[string]roomKey),
		dlChanged: make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// ai-generated: new method, phase 1 multi-room mpq. AddRoom registers the
// outbound sender of a room. Re-registering a roomID replaces its sender (the
// later carrier wins, as with two carriers sharing one room the transport layer
// considers them interchangeable).
func (c *RoomConn) AddRoom(roomID string, sender RoomSender) {
	c.mu.Lock()
	c.rooms[roomID] = sender
	c.mu.Unlock()
}

// ai-generated: new method, phase 1 multi-room mpq. Deliver feeds one inbound
// carrier message from one room to the adapter. In a peer-routing room peerID
// names the remote client; in a 1:1 room it is "". Register it as the carrier's
// OnPeerData (or OnData with peerID "") callback. It copies p (the carrier may
// reuse its buffer) and enqueues it for ReadFrom. If the queue is full or the
// adapter is closed the message is dropped - QUIC recovers dropped packets by
// retransmission.
func (c *RoomConn) Deliver(roomID, peerID string, p []byte) {
	key := roomKey{roomID: roomID, peerID: peerID}
	c.mu.Lock()
	addr, ok := c.keyToAddr[key]
	if !ok {
		addr = roomAddrFor(roomID, peerID)
		c.keyToAddr[key] = addr
		c.addrToKey[addr.String()] = key
	}
	c.mu.Unlock()
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case c.inbox <- roomPacket{data: cp, addr: addr}:
	case <-c.done:
	default:
		// Inbox full: drop, like a saturated datagram socket.
	}
}

// ai-generated: new method, phase 1 multi-room mpq. ReadFrom returns the next
// inbound message and the synthetic address of the (room, peer) it came from.
// It blocks until data arrives, the read deadline fires, or the adapter is
// closed.
func (c *RoomConn) ReadFrom(p []byte) (int, net.Addr, error) {
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

// ai-generated: new method, phase 1 multi-room mpq. WriteTo ships p to the
// (room, peer) named by addr, via that room's registered sender (SendTo(peerID)
// on a peer-routing carrier, Send on a 1:1 carrier). An address never seen
// inbound - no room knows it - is dropped exactly as a datagram to an unknown
// destination would be; QUIC retransmits if needed. It copies p before handing
// it to the carrier so the caller may reuse it.
func (c *RoomConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}
	c.mu.Lock()
	key, ok := c.addrToKey[addr.String()]
	var sender RoomSender
	if ok {
		sender = c.rooms[key.roomID]
	}
	c.mu.Unlock()
	if !ok || sender == nil {
		// Unknown room or no outbound sender yet: drop like a lost datagram.
		return len(p), nil
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	if err := sender.Send(key.peerID, cp); err != nil {
		return 0, err
	}
	return len(p), nil
}

// ai-generated: new method, phase 1 multi-room mpq. Close is idempotent. It
// unblocks any pending ReadFrom and makes further WriteTo fail. It does NOT
// close the underlying carriers (see package doc).
func (c *RoomConn) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	return nil
}

// ai-generated: new method, phase 1 multi-room mpq. LocalAddr returns the fixed
// synthetic local address.
func (c *RoomConn) LocalAddr() net.Addr { return localAddr }

// ai-generated: new method, phase 1 multi-room mpq. SetDeadline sets the read
// deadline (the write side never blocks).
func (c *RoomConn) SetDeadline(t time.Time) error {
	return c.SetReadDeadline(t)
}

// ai-generated: new method, phase 1 multi-room mpq. SetReadDeadline sets the
// deadline for future ReadFrom calls and wakes any ReadFrom currently blocked
// so it observes the new deadline. A zero t clears the deadline. Required:
// quic-go stops the read loop of a borrowed conn by setting a read deadline, so
// Close would hang without it.
func (c *RoomConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	old := c.dlChanged
	c.dlChanged = make(chan struct{})
	c.mu.Unlock()
	close(old)
	return nil
}

// ai-generated: new method, phase 1 multi-room mpq. SetWriteDeadline is a
// no-op: WriteTo hands off to the carrier synchronously.
func (c *RoomConn) SetWriteDeadline(time.Time) error { return nil }
