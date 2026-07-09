package vp8channel

import (
	"bytes"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func pumpPackets(stop <-chan struct{}, from <-chan []byte, to *kcpRuntime) {
	for {
		select {
		case <-stop:
			return
		case pkt := <-from:
			// Strip the on-wire epoch header that kcpConn prepends;
			// the real receive path does this before calling deliver().
			if len(pkt) > epochHdrLen {
				to.deliver(pkt[epochHdrLen:])
			}
		}
	}
}

func checkMessages(t *testing.T, got, want [][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d", len(got), len(want))
	}
	for i, m := range want {
		if !bytes.Equal(got[i], m) {
			t.Errorf("msg %d mismatch: got %d bytes, want %d", i, len(got[i]), len(m))
		}
	}
}

func buildReceiver(n int) (func([]byte), <-chan struct{}, func() [][]byte) {
	var mu sync.Mutex
	var recv [][]byte
	done := make(chan struct{})
	cb := func(msg []byte) {
		mu.Lock()
		recv = append(recv, append([]byte(nil), msg...))
		count := len(recv)
		mu.Unlock()
		if count == n {
			close(done)
		}
	}
	get := func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return recv
	}
	return cb, done, get
}

// TestKCPLoopback runs two KCP runtimes back-to-back through an in-memory
// pipe simulating a perfect carrier. Verifies that messages survive the
// KCP layer with their boundaries intact.
func TestKCPLoopback(t *testing.T) {
	msgs := [][]byte{
		[]byte("hello"),
		bytes.Repeat([]byte("x"), 1000),
		bytes.Repeat([]byte("y"), 20000),
	}

	a2b := make(chan []byte, 256)
	b2a := make(chan []byte, 256)

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

	go pumpPackets(stop, a2b, rtB)
	go pumpPackets(stop, b2a, rtA)

	for _, m := range msgs {
		if err := rtA.send(m); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	select {
	case <-doneB:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for messages")
	}

	checkMessages(t, getRecv(), msgs)
}

func TestVP8KeepaliveDoesNotLookLikeKCP(t *testing.T) {
	if len(vp8Keepalive) != tokenOff {
		t.Errorf("vp8Keepalive length %d != tokenOff %d", len(vp8Keepalive), tokenOff)
	}
}

func TestBatchSampleCarriesMultipleKCPPackets(t *testing.T) {
	hdr := testEpochHdr(1)
	packet := func(payload string) []byte {
		frame := make([]byte, epochHdrLen+len(payload))
		copy(frame, hdr[:])
		copy(frame[epochHdrLen:], payload)
		return frame
	}

	tr := &streamTransport{
		outbound:  make(chan []byte, 4),
		batchSize: 3,
	}
	tr.outbound <- packet("two")
	tr.outbound <- packet("three")
	tr.outbound <- packet("four")

	sample := tr.batchSample(packet("one"))
	if !bytes.Equal(sample[:epochHdrLen], hdr[:]) {
		t.Fatalf("sample epoch header = %x, want %x", sample[:epochHdrLen], hdr[:])
	}

	var got []string
	splitKCPPayload(sample[epochHdrLen:], func(payload []byte) {
		got = append(got, string(payload))
	})
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("split payload count = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("payload[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if left := len(tr.outbound); left != 1 {
		t.Fatalf("outbound left = %d, want 1", left)
	}
}

func TestSplitKCPPayloadAcceptsLegacySinglePacket(t *testing.T) {
	var got [][]byte
	splitKCPPayload([]byte("single"), func(payload []byte) {
		got = append(got, append([]byte(nil), payload...))
	})
	if len(got) != 1 || string(got[0]) != "single" {
		t.Fatalf("split legacy payload = %q", got)
	}
}

func testEpochHdr(epoch uint32) [epochHdrLen]byte {
	var hdr [epochHdrLen]byte
	copy(hdr[:], vp8Keepalive)
	binary.BigEndian.PutUint32(hdr[tokenOff:srcOff], bindingToken("test"))
	binary.BigEndian.PutUint32(hdr[srcOff:dstOff], epoch)
	binary.BigEndian.PutUint32(hdr[dstOff:crcOff], 0)
	binary.BigEndian.PutUint32(hdr[crcOff:epochHdrLen], epochCRC(bindingToken("test"), epoch, 0))
	return hdr
}

func TestHandleIncomingFrameIgnoresLoopedBackLocalEpoch(t *testing.T) {
	tr := &streamTransport{
		bindingToken: bindingToken("test"),
		localEpoch:   12345,
		onData:       func([]byte) {},
	}

	var called atomic.Int32
	tr.reconnectFn = func() { called.Add(1) }

	frame := make([]byte, epochHdrLen+4)
	copy(frame, vp8Keepalive)
	binary.BigEndian.PutUint32(frame[tokenOff:srcOff], tr.bindingToken)
	binary.BigEndian.PutUint32(frame[srcOff:dstOff], tr.localEpoch)
	binary.BigEndian.PutUint32(frame[dstOff:crcOff], 0)
	binary.BigEndian.PutUint32(frame[crcOff:epochHdrLen], epochCRC(tr.bindingToken, tr.localEpoch, 0))
	copy(frame[epochHdrLen:], []byte{1, 2, 3, 4})

	tr.handleIncomingFrame(frame)

	if tr.peerConfirmed.Load() {
		t.Fatal("self-echo frame must not mark peer as seen")
	}
	if got := tr.peerEpoch.Load(); got != 0 {
		t.Fatalf("peer epoch changed on self-echo: got %d want 0", got)
	}
	if got := called.Load(); got != 0 {
		t.Fatalf("reconnect called on self-echo: got %d want 0", got)
	}
}

func TestHandleIncomingFrameIgnoresForeignBindingToken(t *testing.T) {
	tr := &streamTransport{
		bindingToken: bindingToken("srv-client"),
		localEpoch:   12345,
		onData:       func([]byte) {},
	}

	var called atomic.Int32
	tr.reconnectFn = func() { called.Add(1) }

	frame := make([]byte, epochHdrLen+4)
	copy(frame, vp8Keepalive)
	otherToken := bindingToken("other-client")
	binary.BigEndian.PutUint32(frame[tokenOff:srcOff], otherToken)
	binary.BigEndian.PutUint32(frame[srcOff:dstOff], 999)
	binary.BigEndian.PutUint32(frame[dstOff:crcOff], 0)
	binary.BigEndian.PutUint32(frame[crcOff:epochHdrLen], epochCRC(otherToken, 999, 0))
	copy(frame[epochHdrLen:], []byte{1, 2, 3, 4})

	tr.handleIncomingFrame(frame)

	if tr.peerConfirmed.Load() {
		t.Fatal("foreign frame must not mark peer as seen")
	}
	if got := tr.peerEpoch.Load(); got != 0 {
		t.Fatalf("peer epoch changed on foreign frame: got %d want 0", got)
	}
	if got := called.Load(); got != 0 {
		t.Fatalf("reconnect called on foreign frame: got %d want 0", got)
	}
}
