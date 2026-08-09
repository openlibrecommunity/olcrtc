package mpqx_test

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/transport/mpqx"
)

// ai-generated: new test helper type, phase 1 multi-room mpq. recordingSender
// is a RoomSender that records every (peerID, data) it is asked to ship, so a
// test can assert WriteTo routed a packet to the correct room and peer.
type recordingSender struct {
	mu    sync.Mutex
	sents []sent
}

type sent struct {
	peerID string
	data   []byte
}

func (r *recordingSender) Send(peerID string, p []byte) error {
	r.mu.Lock()
	r.sents = append(r.sents, sent{peerID: peerID, data: append([]byte(nil), p...)})
	r.mu.Unlock()
	return nil
}

func (r *recordingSender) all() []sent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sents
}

// ai-generated: new test helper, phase 1 multi-room mpq. readOne delivers one
// packet and returns it with its source address, failing the test on timeout.
func readOne(t *testing.T, conn net.PacketConn) ([]byte, net.Addr) {
	t.Helper()
	buf := make([]byte, 2048)
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	n, addr, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	return buf[:n], addr
}

// ai-generated: new test, phase 1 multi-room mpq. TestRoomConnAddrUniqueness
// checks that distinct (room, peer) pairs get distinct synthetic addresses and
// that the same pair always yields the same address, so quic-go demuxes paths
// and clients into distinct QUIC connections.
func TestRoomConnAddrUniqueness(t *testing.T) {
	rc := mpqx.NewRoom()

	rc.Deliver("room-a", "peer-1", []byte("a1"))
	_, addr1 := readOne(t, rc)
	rc.Deliver("room-a", "peer-2", []byte("a2"))
	_, addr2 := readOne(t, rc)
	rc.Deliver("room-b", "peer-1", []byte("b1"))
	_, addr3 := readOne(t, rc)
	rc.Deliver("room-c", "", []byte("c"))
	_, addr4 := readOne(t, rc)

	if addr1.String() == addr2.String() {
		t.Fatal("same room, different peers must demux to distinct addrs")
	}
	if addr1.String() == addr3.String() {
		t.Fatal("different rooms, same peer must demux to distinct addrs")
	}
	if addr1.String() == addr4.String() {
		t.Fatal("peer-routing addr collides with a 1:1 room addr")
	}
	if addr2.String() == addr3.String() {
		t.Fatal("cross-room addr collision")
	}

	// Re-delivering from an already-seen pair must reuse the same address, so
	// quic-go keeps routing the pair to one connection.
	rc.Deliver("room-a", "peer-1", []byte("a1 again"))
	_, again := readOne(t, rc)
	if again.String() != addr1.String() {
		t.Fatalf("addr for (room-a, peer-1) not stable: got %q, want %q", again, addr1)
	}
	rc.Deliver("room-c", "", []byte("c again"))
	_, again4 := readOne(t, rc)
	if again4.String() != addr4.String() {
		t.Fatalf("addr for (room-c, 1:1) not stable: got %q, want %q", again4, addr4)
	}
}

// TestRoomConnWriteToRouting checks that WriteTo delivers to the exact room and
// peer encoded in the address, and that each room's sender is used.
func TestRoomConnWriteToRouting(t *testing.T) {
	rc := mpqx.NewRoom()
	roomA := &recordingSender{}
	rc.AddRoom("room-a", roomA)
	roomB := &recordingSender{}
	rc.AddRoom("room-b", roomB)

	rc.Deliver("room-a", "peer-1", []byte("in1"))
	rc.Deliver("room-b", "peer-2", []byte("in2"))
	_, addr1 := readOne(t, rc)
	_, addr2 := readOne(t, rc)

	if n, err := rc.WriteTo([]byte("out1"), addr1); err != nil || n != 4 {
		t.Fatalf("WriteTo room-a: n=%d err=%v", n, err)
	}
	if n, err := rc.WriteTo([]byte("out2"), addr2); err != nil || n != 4 {
		t.Fatalf("WriteTo room-b: n=%d err=%v", n, err)
	}

	gotA := roomA.all()
	gotB := roomB.all()
	if len(gotA) != 1 || gotA[0].peerID != "peer-1" || string(gotA[0].data) != "out1" {
		t.Fatalf("room-a sent %+v, want [(peer-1, out1)]", gotA)
	}
	if len(gotB) != 1 || gotB[0].peerID != "peer-2" || string(gotB[0].data) != "out2" {
		t.Fatalf("room-b sent %+v, want [(peer-2, out2)]", gotB)
	}
}

// TestRoomConnUnknownAddrDrop checks that WriteTo to an address that was never
// delivered inbound is dropped like a datagram into the void: no error, no
// send, full length reported.
func TestRoomConnUnknownAddrDrop(t *testing.T) {
	rc := mpqx.NewRoom()
	roomA := &recordingSender{}
	rc.AddRoom("room-a", roomA)

	rc.Deliver("room-a", "peer-1", []byte("in"))
	_, known := readOne(t, rc)

	unknown := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1234}
	if n, err := rc.WriteTo([]byte("elsewhere"), unknown); err != nil || n != 9 {
		t.Fatalf("WriteTo unknown addr: n=%d err=%v (want 9, nil)", n, err)
	}
	// A known addr still routes, proving the drop was about the unknown addr.
	if n, err := rc.WriteTo([]byte("known"), known); err != nil || n != 5 {
		t.Fatalf("WriteTo known addr: n=%d err=%v (want 5, nil)", n, err)
	}
	if got := len(roomA.all()); got != 1 {
		t.Fatalf("room-a sent %d packets, want exactly the known-addr one", got)
	}
}

// TestRoomConnDeadlineWakeup checks that a read deadline fires for a blocked
// ReadFrom and that clearing it restores blocking.
func TestRoomConnDeadlineWakeup(t *testing.T) {
	rc := mpqx.NewRoom()
	if err := rc.SetReadDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 64)
	_, _, err := rc.ReadFrom(buf)
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("ReadFrom after deadline = %v, want net timeout", err)
	}

	// Clearing the deadline lets a subsequent ReadFrom block again; a late
	// deliver wakes it with data instead of an error.
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear read deadline: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, rerr := rc.ReadFrom(buf)
		done <- rerr
	}()
	time.Sleep(20 * time.Millisecond)
	rc.Deliver("room-a", "peer-1", []byte("hi"))
	if err := <-done; err != nil {
		t.Fatalf("ReadFrom after clearing deadline: %v", err)
	}
}

// TestRoomConnCloseUnblocksRead checks that Close terminates a blocked ReadFrom
// with net.ErrClosed.
func TestRoomConnCloseUnblocksRead(t *testing.T) {
	rc := mpqx.NewRoom()
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		_, _, err := rc.ReadFrom(buf)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-done; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("ReadFrom after Close = %v, want net.ErrClosed", err)
	}
	// Close is idempotent and Deliver after Close is a silent drop.
	if err := rc.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	rc.Deliver("room-a", "peer-1", []byte("late"))
	if _, err := rc.WriteTo([]byte("x"), &net.UDPAddr{IP: net.IPv4zero}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("WriteTo after Close = %v, want net.ErrClosed", err)
	}
}

// TestRoomConnInboxOverflowDrop checks that a full inbox drops the newest
// packet instead of blocking or growing without bound.
func TestRoomConnInboxOverflowDrop(t *testing.T) {
	rc := mpqx.NewRoom()
	// Far above the inbox capacity: the first 1024 fit, the rest are dropped.
	const total = 2048
	for i := 0; i < total; i++ {
		rc.Deliver("room-a", "peer-1", []byte{byte(i)})
	}
	buf := make([]byte, 64)
	if err := rc.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	// The queue holds exactly cap packets; every read succeeds until it drains.
	got := 0
	for {
		n, _, err := rc.ReadFrom(buf)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				break
			}
			t.Fatalf("ReadFrom: %v", err)
		}
		if n != 1 {
			t.Fatalf("packet length = %d, want 1", n)
		}
		got++
	}
	if got >= total {
		t.Fatalf("inbox admitted all %d packets, want overflow to have dropped some", total)
	}
	if got == 0 {
		t.Fatal("inbox admitted nothing")
	}
}

// TestRoomConnConcurrentSmoke drives Deliver/ReadFrom/WriteTo from several
// goroutines at once; -race validates the locking. The inbox may legitimately
// drop packets when the writers outpace the reader (drop-on-full semantics),
// so the test asserts liveness (traffic flowed, no lockup) rather than a
// packet count.
func TestRoomConnConcurrentSmoke(t *testing.T) {
	rc := mpqx.NewRoom()
	roomA := &recordingSender{}
	rc.AddRoom("room-a", roomA)

	const writers = 4
	const perWriter = 300
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		w := i
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				rc.Deliver("room-a", "peer-1", []byte{byte(w), byte(j)})
			}
		}()
	}

	// Drain for as long as writers may still deliver, then exit once they are
	// done and the inbox briefly idles; WriteTo the returned addr races with the
	// writers (exercising the addr->room maps and the sender path).
	writersDone := make(chan struct{})
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 64)
		for {
			_ = rc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, addr, err := rc.ReadFrom(buf)
			if err != nil {
				select {
				case <-writersDone:
					return
				default:
					continue // writers may still deliver; keep draining
				}
			}
			if _, werr := rc.WriteTo(buf[:n], addr); werr != nil {
				t.Errorf("WriteTo raced: %v", werr)
				return
			}
		}
	}()
	wg.Wait()
	close(writersDone)
	<-readDone

	if len(roomA.all()) == 0 {
		t.Fatal("room-a shipped nothing under concurrent load")
	}
}
