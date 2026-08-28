package vp8channel

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/openlibrecommunity/olcrtc/internal/engine"
	enginebuiltin "github.com/openlibrecommunity/olcrtc/internal/engine/builtin"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
)

var errVP8UnitBoom = errors.New("boom")

// TestControlEpochTracksDataEpoch guards the issue #95 multi-client invariant:
// the control-plane epoch is derived live from the data epoch as
// localEpoch|controlEpochFlag. This lets the server correlate a client's data
// and control planes by arithmetic (controlEpoch &^ controlEpochFlag ==
// dataEpoch), which is what keys the per-peer control sessions. The two planes
// rotate together on reconnect; the control epoch always carries the high bit
// and always shares the current data epoch's low bits.
func TestControlEpochTracksDataEpoch(t *testing.T) {
	tr := &streamTransport{
		bindingToken: bindingToken("room-95"),
		localEpoch:   randomEpoch(),
	}

	check := func(stage string) {
		data := tr.localEpochValue()
		ctrl := tr.controlEpochValue()
		if ctrl&controlEpochFlag == 0 {
			t.Fatalf("%s: control epoch 0x%08x missing control flag", stage, ctrl)
		}
		if ctrl != data|controlEpochFlag {
			t.Fatalf("%s: control epoch 0x%08x != data 0x%08x | flag", stage, ctrl, data)
		}
		if ctrl&^controlEpochFlag != data {
			t.Fatalf("%s: control epoch does not correlate to data epoch 0x%08x", stage, data)
		}
		hdr := tr.controlEpochHeader()
		_, hdrEpoch, _, ok := parseEpochHeader(hdr[:])
		if !ok {
			t.Fatalf("%s: control epoch header failed to parse", stage)
		}
		if hdrEpoch != ctrl {
			t.Fatalf("%s: control wire epoch 0x%08x != controlEpochValue 0x%08x", stage, hdrEpoch, ctrl)
		}
	}

	check("initial")
	// Both planes rotate together across reconnects.
	for range 5 {
		tr.rotateEpochHeader()
		check("after rotation")
	}
}

func TestWriterCadenceStaysAtFrameInterval(t *testing.T) {
	tr := &streamTransport{
		frameInterval: time.Second / 60,
		batchSize:     64,
	}
	if got := tr.frameInterval; got != time.Second/60 {
		t.Fatalf("frameInterval = %v, want %v", got, time.Second/60)
	}

	tr.batchSize = 1
	if got := tr.frameInterval; got != time.Second/60 {
		t.Fatalf("frameInterval after batch change = %v, want %v", got, time.Second/60)
	}
}

type fakeVideoStream struct {
	connectErr error
	closeErr   error
	canSend    bool
	trackAdded bool
	trackCB    func(*webrtc.TrackRemote, *webrtc.RTPReceiver)
	reconnect  func()
	should     func() bool
	ended      func(string)
	watched    bool
	closed     bool

	reconnects atomic.Int32
}

func (s *fakeVideoStream) Connect(context.Context) error { return s.connectErr }
func (s *fakeVideoStream) Close() error {
	s.closed = true
	return s.closeErr
}
func (s *fakeVideoStream) SetReconnectCallback(cb func())    { s.reconnect = cb }
func (s *fakeVideoStream) SetShouldReconnect(fn func() bool) { s.should = fn }
func (s *fakeVideoStream) SetEndedCallback(cb func(string))  { s.ended = cb }
func (s *fakeVideoStream) WatchConnection(context.Context)   { s.watched = true }
func (s *fakeVideoStream) CanSend() bool                     { return s.canSend }
func (s *fakeVideoStream) SubscriberCanSend() bool           { return s.canSend }
func (s *fakeVideoStream) AddTrack(webrtc.TrackLocal) error  { s.trackAdded = true; return nil }
func (s *fakeVideoStream) Reconnect(string)                  { s.reconnects.Add(1) }
func (s *fakeVideoStream) SetTrackHandler(cb func(*webrtc.TrackRemote, *webrtc.RTPReceiver)) {
	s.trackCB = cb
}

// fakeEngineSession adapts fakeVideoStream so it satisfies engine.Session and
// engine.VideoTrackCapable, the two interfaces the vp8channel transport
// looks up after the provider-layer collapse.
type fakeEngineSession struct {
	stream *fakeVideoStream
}

func (s *fakeEngineSession) Connect(ctx context.Context) error { return s.stream.Connect(ctx) }
func (s *fakeEngineSession) Send([]byte) error                 { return nil }
func (s *fakeEngineSession) Close() error                      { return s.stream.Close() }
func (s *fakeEngineSession) SetReconnectCallback(cb func())    { s.stream.SetReconnectCallback(cb) }
func (s *fakeEngineSession) SetShouldReconnect(fn func() bool) { s.stream.SetShouldReconnect(fn) }
func (s *fakeEngineSession) SetEndedCallback(cb func(string))  { s.stream.SetEndedCallback(cb) }
func (s *fakeEngineSession) WatchConnection(ctx context.Context) {
	s.stream.WatchConnection(ctx)
}
func (s *fakeEngineSession) CanSend() bool                           { return s.stream.CanSend() }
func (s *fakeEngineSession) SubscriberCanSend() bool                 { return s.stream.SubscriberCanSend() }
func (s *fakeEngineSession) GetBufferedAmount() uint64               { return 0 }
func (s *fakeEngineSession) Reconnect(string)                        {}
func (s *fakeEngineSession) AddVideoTrack(t webrtc.TrackLocal) error { return s.stream.AddTrack(t) }
func (s *fakeEngineSession) SetVideoTrackHandler(cb func(*webrtc.TrackRemote, *webrtc.RTPReceiver)) {
	s.stream.SetTrackHandler(cb)
}

type noVideoEngineSession struct {
	engine.Session
}

func TestNewConnectSendCallbacksFeaturesAndClose(t *testing.T) {
	stream := &fakeVideoStream{canSend: true}
	name := "vp8channel-unit-new"
	enginebuiltin.Register(name, func(context.Context, enginebuiltin.Config) (engine.Session, error) {
		return &fakeEngineSession{stream: stream}, nil
	})

	trIface, err := New(context.Background(), transport.Config{
		Provider: name,
		DeviceID: "client",
		Options:  Options{FPS: 30, BatchSize: 1},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	tr, ok := trIface.(*streamTransport)
	if !ok {
		t.Fatalf("transport type = %T, want *streamTransport", trIface)
	}
	if !stream.trackAdded || stream.trackCB == nil {
		t.Fatal("New() did not attach track and handler")
	}
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if tr.data.get() == nil || !tr.writerUp.Load() {
		t.Fatal("Connect() should eagerly initialize kcp and writer")
	}
	tr.SetReconnectCallback(func() {})
	tr.SetShouldReconnect(func() bool { return true })
	tr.SetEndedCallback(func(string) {})
	tr.WatchConnection(context.Background())
	if stream.reconnect == nil || stream.should == nil || stream.ended == nil || !stream.watched {
		t.Fatal("callbacks/watch were not forwarded")
	}

	peerEpoch := uint32(0x200)
	firstFrame := make([]byte, epochHdrLen+4)
	copy(firstFrame, vp8Keepalive)
	binary.BigEndian.PutUint32(firstFrame[tokenOff:srcOff], tr.bindingToken)
	binary.BigEndian.PutUint32(firstFrame[srcOff:dstOff], peerEpoch)
	binary.BigEndian.PutUint32(firstFrame[dstOff:crcOff], 0)
	binary.BigEndian.PutUint32(firstFrame[crcOff:epochHdrLen], epochCRC(tr.bindingToken, peerEpoch, 0))
	copy(firstFrame[epochHdrLen:], "data")
	tr.handleIncomingFrame(firstFrame)
	if tr.data.get() == nil {
		t.Fatal("kcp not initialized after first peer frame")
	}

	if !tr.CanSend() {
		t.Fatal("CanSend() = false, want true")
	}
	if features := tr.Features(); features.MaxPayloadSize == 0 {
		t.Fatalf("Features() = %+v", features)
	}
	if err := tr.Send([]byte("payload")); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	tr.data.drain()
	if err := tr.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := tr.Send([]byte("closed")); !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("Send(closed) error = %v, want %v", err, ErrTransportClosed)
	}
}

func TestNewErrorPaths(t *testing.T) {
	enginebuiltin.Register("vp8channel-create-fails", func(context.Context, enginebuiltin.Config) (engine.Session, error) {
		return nil, errVP8UnitBoom
	})
	_, err := New(context.Background(), transport.Config{Provider: "vp8channel-create-fails"})
	if err == nil || err.Error() != "open engine session: boom" {
		t.Fatalf("New() error = %v", err)
	}

	enginebuiltin.Register("vp8channel-no-video", func(context.Context, enginebuiltin.Config) (engine.Session, error) {
		return &noVideoEngineSession{Session: &fakeEngineSession{stream: &fakeVideoStream{}}}, nil
	})
	_, err = New(context.Background(), transport.Config{Provider: "vp8channel-no-video"})
	if !errors.Is(err, ErrVideoTrackUnsupported) {
		t.Fatalf("New() error = %v, want %v", err, ErrVideoTrackUnsupported)
	}
}

func TestEpochHeaderTokenAndOutboundCapacity(t *testing.T) {
	tr := &streamTransport{
		stream:       &fakeVideoStream{canSend: true},
		data:         newKCPPlane(10, nil),
		control:      newKCPPlane(1, nil),
		closeCh:      make(chan struct{}),
		writerDone:   make(chan struct{}),
		bindingToken: bindingToken("client"),
		localEpoch:   0x01020304,
	}

	hdr := tr.epochHeader()
	if !bytes.Equal(hdr[:tokenOff], vp8Keepalive) ||
		binary.BigEndian.Uint32(hdr[tokenOff:srcOff]) != tr.bindingToken ||
		binary.BigEndian.Uint32(hdr[srcOff:dstOff]) != tr.localEpoch ||
		binary.BigEndian.Uint32(hdr[dstOff:crcOff]) != 0 ||
		binary.BigEndian.Uint32(hdr[crcOff:epochHdrLen]) != epochCRC(tr.bindingToken, tr.localEpoch, 0) {
		t.Fatalf("epochHeader() = %x", hdr)
	}
	if bindingToken("") == 0 || randomEpoch() == 0 {
		t.Fatal("bindingToken/randomEpoch returned zero")
	}

	rt, err := startKCP(tr.data.out, nil, tr.epochHeader())
	if err != nil {
		t.Fatalf("startKCP: %v", err)
	}
	defer rt.close()
	tr.data.set(rt)

	for len(tr.data.out) < cap(tr.data.out)*canSendHighWatermark/100 {
		tr.data.out <- &packetBuffer{data: []byte("queued")}
	}
	if tr.CanSend() {
		t.Fatal("CanSend() = true at high watermark")
	}
	tr.data.drain()
	if !tr.CanSend() {
		t.Fatal("CanSend() = false after drain")
	}
	tr.closed.Store(true)
	if tr.CanSend() {
		t.Fatal("CanSend() = true after closed")
	}
}

func TestResetPeerRestartsKCPAndDrainsOutbound(t *testing.T) {
	tr := &streamTransport{
		stream:       &fakeVideoStream{canSend: true},
		data:         newKCPPlane(10, nil),
		control:      newKCPPlane(10, nil),
		closeCh:      make(chan struct{}),
		writerDone:   make(chan struct{}),
		bindingToken: bindingToken("client"),
		localEpoch:   0x01020304,
	}
	defer func() {
		_ = tr.Close()
	}()

	rt, err := startKCP(tr.data.out, nil, tr.epochHeader())
	if err != nil {
		t.Fatalf("startKCP: %v", err)
	}
	tr.data.set(rt)
	tr.data.out <- &packetBuffer{data: []byte("stale")}
	oldEpoch := tr.localEpoch

	tr.ResetPeer()

	got := tr.data.get()
	if got == nil || got == rt {
		t.Fatalf("ResetPeer kcp = %p, want fresh non-nil runtime distinct from %p", got, rt)
	}
	if len(tr.data.out) != 0 {
		t.Fatalf("ResetPeer left %d outbound frame(s), want 0", len(tr.data.out))
	}
	if tr.localEpoch == oldEpoch {
		t.Fatalf("ResetPeer localEpoch = %#x, want different epoch", tr.localEpoch)
	}
	select {
	case <-rt.readDone:
	case <-time.After(time.Second):
		t.Fatal("old KCP runtime did not stop")
	}
}

func TestVP8FrameStateAssemblesAndRejectsCorruptFrames(t *testing.T) {
	frame := append(append([]byte(nil), vp8Keepalive...), bytes.Repeat([]byte{0x01}, epochHdrLen-len(vp8Keepalive))...)
	var state vp8FrameState

	got := state.processRTPPacket(&rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 10, Marker: true},
		Payload: append([]byte{0x10}, frame...),
	})
	if !bytes.Equal(got, frame) {
		t.Fatalf("single-packet frame = %x, want %x", got, frame)
	}

	state = vp8FrameState{}
	if partial := state.processRTPPacket(&rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 20},
		Payload: append([]byte{0x10}, frame[:4]...),
	}); partial != nil {
		t.Fatalf("partial frame = %x, want nil", partial)
	}
	got = state.processRTPPacket(&rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 21, Marker: true},
		Payload: append([]byte{0x00}, frame[4:]...),
	})
	if !bytes.Equal(got, frame) {
		t.Fatalf("fragmented frame = %x, want %x", got, frame)
	}

	state = vp8FrameState{}
	_ = state.processRTPPacket(&rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 30},
		Payload: append([]byte{0x10}, frame[:4]...),
	})
	if gapped := state.processRTPPacket(&rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 32, Marker: true},
		Payload: append([]byte{0x00}, frame[4:]...),
	}); gapped != nil {
		t.Fatalf("frame after sequence gap = %x, want nil", gapped)
	}

	state = vp8FrameState{}
	if bad := state.processRTPPacket(&rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 40, Marker: true},
		Payload: []byte{},
	}); bad != nil {
		t.Fatalf("bad vp8 payload = %x, want nil", bad)
	}
}

func TestHandleIncomingFrameEpochFilteringAndReconnect(t *testing.T) {
	called := 0
	tr := &streamTransport{
		stream:       &fakeVideoStream{canSend: true},
		data:         newKCPPlane(16, nil),
		control:      newKCPPlane(16, nil),
		closeCh:      make(chan struct{}),
		writerDone:   make(chan struct{}),
		bindingToken: bindingToken("client"),
		localEpoch:   0x100,
		onData:       func([]byte) { called++ },
	}
	defer func() {
		_ = tr.Close()
	}()

	mkFrame := func(token, epoch uint32, payload []byte) []byte {
		frame := make([]byte, epochHdrLen+len(payload))
		copy(frame, vp8Keepalive)
		binary.BigEndian.PutUint32(frame[tokenOff:srcOff], token)
		binary.BigEndian.PutUint32(frame[srcOff:dstOff], epoch)
		binary.BigEndian.PutUint32(frame[dstOff:crcOff], 0)
		binary.BigEndian.PutUint32(frame[crcOff:epochHdrLen], epochCRC(token, epoch, 0))
		copy(frame[epochHdrLen:], payload)
		return frame
	}

	tr.handleIncomingFrame(mkFrame(bindingToken("other"), 1, []byte("x")))
	tr.handleIncomingFrame(mkFrame(tr.bindingToken, tr.localEpoch, []byte("self")))
	if tr.peerConfirmed.Load() || called != 0 {
		t.Fatal("filtered frames changed peer state")
	}

	// Neither a keepalive nor a non-empty broadcast may bind the peer.
	tr.handleIncomingFrame(mkFrame(tr.bindingToken, 1, nil))
	tr.handleIncomingFrame(mkFrame(tr.bindingToken, 2, []byte("foreign-data")))
	if tr.peerConfirmed.Load() || tr.peerEpoch.Load() != 0 {
		t.Fatal("unauthenticated frames changed peer state")
	}
	if err := tr.ConfirmPeer("00000001"); err != nil {
		t.Fatalf("ConfirmPeer() error = %v", err)
	}
	if tr.peerEpoch.Load() != 1 {
		t.Fatalf("peer epoch not stored: got %d want 1", tr.peerEpoch.Load())
	}

	reconnected := false
	tr.SetReconnectCallback(func() { reconnected = true })
	stream, ok := tr.stream.(*fakeVideoStream)
	if !ok {
		t.Fatalf("stream type = %T, want *fakeVideoStream", tr.stream)
	}
	if stream.reconnect == nil {
		t.Fatal("SetReconnectCallback did not install stream callback")
	}
	stream.reconnect()
	if !reconnected || tr.data.get() == nil {
		t.Fatalf("stream reconnect did not reset/callback: reconnected=%v kcp=%v", reconnected, tr.data.get())
	}
	reconnected = false
	// After reconnect, only the next authenticated welcome can bind a peer.
	if tr.peerConfirmed.Load() {
		t.Fatal("reconnect should reset peerConfirmed")
	}
	tr.handleIncomingFrame(mkFrame(tr.bindingToken, 2, []byte("new-peer-after-reconnect")))
	if tr.peerConfirmed.Load() || tr.peerEpoch.Load() != 0 {
		t.Fatal("frame after reconnect bound an unauthenticated peer")
	}
	if err := tr.ConfirmPeer("00000002"); err != nil {
		t.Fatalf("ConfirmPeer() after reconnect error = %v", err)
	}
	if tr.peerEpoch.Load() != 2 {
		t.Fatalf("peer epoch not re-latched: got %d want 2", tr.peerEpoch.Load())
	}
}

func TestClientsCannotBindEachOtherBeforeServerWelcome(t *testing.T) {
	token := bindingToken("shared-room")
	first := &streamTransport{bindingToken: token, localEpoch: 0x101}
	second := &streamTransport{bindingToken: token, localEpoch: 0x202}

	first.handleIncomingFrame(mkPeerFrame(token, second.localEpochValue(), nil))
	first.handleIncomingFrame(mkPeerFrame(token, second.localEpochValue(), []byte("client-two")))
	second.handleIncomingFrame(mkPeerFrame(token, first.localEpochValue(), nil))
	second.handleIncomingFrame(mkPeerFrame(token, first.localEpochValue(), []byte("client-one")))

	for name, client := range map[string]*streamTransport{"first": first, "second": second} {
		if client.peerConfirmed.Load() || client.peerEpoch.Load() != 0 {
			t.Fatalf("%s client bound to another unauthenticated client", name)
		}
	}
}

// mkPeerFrame builds a broadcast data-plane frame (dst=0) from epoch on token,
// carrying payload.
func mkPeerFrame(token, epoch uint32, payload []byte) []byte {
	frame := make([]byte, epochHdrLen+len(payload))
	copy(frame, vp8Keepalive)
	binary.BigEndian.PutUint32(frame[tokenOff:srcOff], token)
	binary.BigEndian.PutUint32(frame[srcOff:dstOff], epoch)
	binary.BigEndian.PutUint32(frame[dstOff:crcOff], 0)
	binary.BigEndian.PutUint32(frame[crcOff:epochHdrLen], epochCRC(token, epoch, 0))
	copy(frame[epochHdrLen:], payload)
	return frame
}

// below, peer-restart-corroboration PR (rest of the test predates it).
// TestPeerRestartRebuildsProviderAfterGrace guards issue #105: when the latched
// peer goes silent past peerRestartGrace and a frame from a fresh epoch
// arrives, the transport rebuilds the provider (stream.Reconnect) so the client
// re-handshakes against the restarted server instead of stalling for the full
// control-liveness window.
func TestPeerRestartRebuildsProviderAfterGrace(t *testing.T) {
	stream := &fakeVideoStream{canSend: true}
	tr := &streamTransport{
		stream:           stream,
		data:             newKCPPlane(16, nil),
		control:          newKCPPlane(16, nil),
		closeCh:          make(chan struct{}),
		writerDone:       make(chan struct{}),
		bindingToken:     bindingToken("client"),
		localEpoch:       0x100,
		peerRestartGrace: 20 * time.Millisecond,
	}
	defer func() { _ = tr.Close() }()

	if err := tr.ConfirmPeer("00000200"); err != nil {
		t.Fatalf("ConfirmPeer() error = %v", err)
	}
	if tr.peerEpoch.Load() != 0x200 {
		t.Fatalf("peer epoch = 0x%08x, want 0x200", tr.peerEpoch.Load())
	}

	// A different epoch inside the grace window must NOT rebuild the provider.
	tr.handleIncomingFrame(mkPeerFrame(tr.bindingToken, 0x300, []byte("early")))
	time.Sleep(10 * time.Millisecond)
	if got := stream.reconnects.Load(); got != 0 {
		t.Fatalf("provider rebuilt inside grace window: got %d, want 0", got)
	}

	// After the latched peer has been silent past the grace window, a frame
	// from the new epoch is read as a restart and rebuilds the provider - but
	// only once the control-plane liveness loop has corroborated trouble.
	time.Sleep(15 * time.Millisecond)
	tr.NotifyLinkHealth(true)
	tr.handleIncomingFrame(mkPeerFrame(tr.bindingToken, 0x300, []byte("restart")))
	deadline := time.Now().Add(time.Second)
	for stream.reconnects.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := stream.reconnects.Load(); got != 1 {
		t.Fatalf("provider rebuilds after grace = %d, want 1", got)
	}
	if !tr.peerRestarting.Load() {
		t.Fatal("peerRestarting flag not set after restart detection")
	}
}

// peer-restart-corroboration PR (rest of the test predates it).
// TestPeerRestartRebuildsOnlyOnce ensures repeated frames from the new epoch do
// not trigger a rebuild storm before the latch is reset.
func TestPeerRestartRebuildsOnlyOnce(t *testing.T) {
	stream := &fakeVideoStream{canSend: true}
	tr := &streamTransport{
		stream:           stream,
		data:             newKCPPlane(16, nil),
		control:          newKCPPlane(16, nil),
		closeCh:          make(chan struct{}),
		writerDone:       make(chan struct{}),
		bindingToken:     bindingToken("client"),
		localEpoch:       0x100,
		peerRestartGrace: 10 * time.Millisecond,
	}
	defer func() { _ = tr.Close() }()

	if err := tr.ConfirmPeer("00000200"); err != nil {
		t.Fatalf("ConfirmPeer() error = %v", err)
	}
	time.Sleep(15 * time.Millisecond)
	tr.NotifyLinkHealth(true)
	for range 5 {
		tr.handleIncomingFrame(mkPeerFrame(tr.bindingToken, 0x300, []byte("restart")))
	}
	time.Sleep(50 * time.Millisecond)
	if got := stream.reconnects.Load(); got != 1 {
		t.Fatalf("provider rebuilt %d times, want exactly 1", got)
	}
}

// peer-restart-corroboration PR (rest of the test predates it).
// TestLivePeerKeepsLatchFresh confirms a peer that keeps sending frames within
// the grace window never trips the restart watchdog, even if a stray frame from
// another epoch shows up (unrelated room participant).
func TestLivePeerKeepsLatchFresh(t *testing.T) {
	stream := &fakeVideoStream{canSend: true}
	tr := &streamTransport{
		stream:           stream,
		data:             newKCPPlane(16, nil),
		control:          newKCPPlane(16, nil),
		closeCh:          make(chan struct{}),
		writerDone:       make(chan struct{}),
		bindingToken:     bindingToken("client"),
		localEpoch:       0x100,
		peerRestartGrace: 40 * time.Millisecond,
	}
	defer func() { _ = tr.Close() }()

	if tr.linkUnhealthy.Load() {
		t.Fatal("linkUnhealthy should default to false")
	}

	if err := tr.ConfirmPeer("00000200"); err != nil {
		t.Fatalf("ConfirmPeer() error = %v", err)
	}
	// Keep the latched peer alive with frequent keepalives while a foreign
	// epoch repeatedly shows up. The latch stays fresh, so no rebuild fires.
	for range 8 {
		tr.handleIncomingFrame(mkPeerFrame(tr.bindingToken, 0x200, nil))
		tr.handleIncomingFrame(mkPeerFrame(tr.bindingToken, 0x999, []byte("noise")))
		time.Sleep(10 * time.Millisecond)
	}
	if got := stream.reconnects.Load(); got != 0 {
		t.Fatalf("provider rebuilt %d times for a live peer, want 0", got)
	}
}

// TestPeerRestartSuppressedWhenControlHealthy reproduces the multi-client SFU
// scenario directly: a second, unrelated room participant's epoch shows up
// after the latched peer's silence exceeds peerRestartGrace, but the client's
// own control-plane liveness never reported trouble. The heuristic must not
// tear down a perfectly healthy provider over unrelated room noise.
func TestPeerRestartSuppressedWhenControlHealthy(t *testing.T) {
	stream := &fakeVideoStream{canSend: true}
	tr := &streamTransport{
		stream:           stream,
		data:             newKCPPlane(16, nil),
		control:          newKCPPlane(16, nil),
		closeCh:          make(chan struct{}),
		writerDone:       make(chan struct{}),
		bindingToken:     bindingToken("client"),
		localEpoch:       0x100,
		peerRestartGrace: 10 * time.Millisecond,
	}
	defer func() { _ = tr.Close() }()

	// Bind the real server epoch, then let it go quiet past the grace
	// window - a normal, brief silence, not a real restart.
	if err := tr.ConfirmPeer("00000200"); err != nil {
		t.Fatalf("ConfirmPeer() error = %v", err)
	}
	time.Sleep(15 * time.Millisecond)

	// A second olcbox client joins the same Telemost room; the SFU broadcasts
	// its epoch to everyone, us included. linkUnhealthy is never set.
	tr.handleIncomingFrame(mkPeerFrame(tr.bindingToken, 0x0a301844, []byte("second client")))
	time.Sleep(50 * time.Millisecond)

	if got := stream.reconnects.Load(); got != 0 {
		t.Fatalf("provider rebuilt %d times for an unrelated peer with healthy control plane, want 0", got)
	}
}

// TestPeerRestartFiresOnceCorroborated confirms NotifyLinkHealth(true) is a
// gate, not a permanent disable: with corroborating evidence the client's own
// link is down, the same foreign-epoch frame still triggers the fast-path
// provider rebuild.
func TestPeerRestartFiresOnceCorroborated(t *testing.T) {
	stream := &fakeVideoStream{canSend: true}
	tr := &streamTransport{
		stream:           stream,
		data:             newKCPPlane(16, nil),
		control:          newKCPPlane(16, nil),
		closeCh:          make(chan struct{}),
		writerDone:       make(chan struct{}),
		bindingToken:     bindingToken("client"),
		localEpoch:       0x100,
		peerRestartGrace: 10 * time.Millisecond,
	}
	defer func() { _ = tr.Close() }()

	if err := tr.ConfirmPeer("00000200"); err != nil {
		t.Fatalf("ConfirmPeer() error = %v", err)
	}
	time.Sleep(15 * time.Millisecond)
	tr.NotifyLinkHealth(true)
	tr.handleIncomingFrame(mkPeerFrame(tr.bindingToken, 0x300, []byte("restart")))

	deadline := time.Now().Add(time.Second)
	for stream.reconnects.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := stream.reconnects.Load(); got != 1 {
		t.Fatalf("provider rebuilds when corroborated = %d, want 1", got)
	}
}

// TestNotifyLinkHealthTogglesGuard is a direct unit test of the setter.
func TestNotifyLinkHealthTogglesGuard(t *testing.T) {
	tr := &streamTransport{}
	if tr.linkUnhealthy.Load() {
		t.Fatal("zero-value linkUnhealthy should be false")
	}
	tr.NotifyLinkHealth(true)
	if !tr.linkUnhealthy.Load() {
		t.Fatal("NotifyLinkHealth(true) did not set linkUnhealthy")
	}
	tr.NotifyLinkHealth(false)
	if tr.linkUnhealthy.Load() {
		t.Fatal("NotifyLinkHealth(false) did not clear linkUnhealthy")
	}
}

func pushedSeqs(buffer *reorderBuffer, packet *rtp.Packet) []uint16 {
	var out []uint16
	buffer.push(packet, func(delivered *rtp.Packet) {
		out = append(out, delivered.SequenceNumber)
	})
	return out
}

func TestReorderBufferRestoresOrderAndSurvivesLoss(t *testing.T) {
	// In-order packets pass straight through.
	b := newReorderBuffer()
	got := make([]uint16, 0, 3)
	for _, seq := range []uint16{100, 101, 102} {
		got = append(got, pushedSeqs(b, &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}})...)
	}
	if !reflect.DeepEqual(got, []uint16{100, 101, 102}) {
		t.Fatalf("in-order drain = %v, want [100 101 102]", got)
	}

	// A reordered packet is held until the gap fills, then both drain in order.
	b = newReorderBuffer()
	if out := pushedSeqs(b, &rtp.Packet{Header: rtp.Header{SequenceNumber: 10}}); !reflect.DeepEqual(out, []uint16{10}) {
		t.Fatalf("first packet = %v, want [10]", out)
	}
	// 12 arrives before 11: must be buffered, nothing delivered yet.
	if out := pushedSeqs(b, &rtp.Packet{Header: rtp.Header{SequenceNumber: 12}}); out != nil {
		t.Fatalf("out-of-order packet drained early = %v, want nil", out)
	}
	// 11 fills the hole: 11 and 12 drain in order.
	out := pushedSeqs(b, &rtp.Packet{Header: rtp.Header{SequenceNumber: 11}})
	if !reflect.DeepEqual(out, []uint16{11, 12}) {
		t.Fatalf("gap fill drain = %v, want [11 12]", out)
	}

	// Genuine loss: a full window piles up behind a hole, buffer skips the
	// lost sequence rather than stalling forever.
	b = newReorderBuffer()
	_ = pushedSeqs(b, &rtp.Packet{Header: rtp.Header{SequenceNumber: 0}})
	var delivered int
	for i := 2; i <= reorderWindow+2; i++ {
		seq := uint16(i & 0xffff)
		delivered += len(pushedSeqs(b, &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}}))
	}
	if delivered == 0 {
		t.Fatal("buffer stalled on lost packet: nothing delivered after window overflow")
	}

	// Stale packets older than the current position are dropped.
	b = newReorderBuffer()
	_ = pushedSeqs(b, &rtp.Packet{Header: rtp.Header{SequenceNumber: 50}})
	if out := pushedSeqs(b, &rtp.Packet{Header: rtp.Header{SequenceNumber: 49}}); out != nil {
		t.Fatalf("stale packet delivered = %v, want nil", out)
	}
}

func TestReorderBufferReusesPacketStorage(t *testing.T) {
	buffer := newReorderBuffer()
	firstInput := &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 1},
		Payload: []byte("first"),
	}
	var first *rtp.Packet
	buffer.push(firstInput, func(packet *rtp.Packet) {
		first = packet
		if string(packet.Payload) != "first" {
			t.Fatalf("first payload = %q", packet.Payload)
		}
	})
	secondInput := &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 2},
		Payload: []byte("second"),
	}
	buffer.push(secondInput, func(packet *rtp.Packet) {
		if packet != first {
			t.Fatal("reorder buffer did not reuse delivered packet storage")
		}
		if string(packet.Payload) != "second" {
			t.Fatalf("second payload = %q", packet.Payload)
		}
	})
}

// TestPeerSessionForConcurrentFirstContactConverges guards olcrtc#142: two
// goroutines racing to create the session for a brand-new peer epoch (which
// happens for real when independent RTP reader goroutines deliver that
// peer's first data and control frames back to back) must converge on
// exactly one *peerSession instead of each minting its own KCP runtime and
// clobbering the other in the table.
func TestPeerSessionForConcurrentFirstContactConverges(t *testing.T) {
	tr := &streamTransport{
		stream:        &fakeVideoStream{canSend: true},
		closeCh:       make(chan struct{}),
		writerDone:    make(chan struct{}),
		bindingToken:  bindingToken("room-142"),
		localEpoch:    0x100,
		serverMode:    true,
		frameInterval: time.Millisecond,
		onPeerData:    func(string, []byte) {},
	}
	defer func() {
		close(tr.closeCh)
		tr.peers.closeAll()
	}()

	const goroutines = 32
	const epoch = 0x9a301844

	results := make(chan *peerSession, goroutines)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for range goroutines {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			results <- tr.peerSessionFor(epoch)
		}()
	}
	start.Done()
	done.Wait()
	close(results)

	var first *peerSession
	for sess := range results {
		if sess == nil {
			t.Fatal("peerSessionFor returned nil under contention")
		}
		if first == nil {
			first = sess
			continue
		}
		if sess != first {
			t.Fatalf("peerSessionFor returned distinct sessions for the same epoch: %p != %p", sess, first)
		}
	}

	if got := tr.peers.len(); got != 1 {
		t.Fatalf("peer table has %d sessions for one epoch, want 1", got)
	}
}

func TestSeqLessWrapAround(t *testing.T) {
	cases := []struct {
		a, b uint16
		want bool
	}{
		{1, 2, true},
		{2, 1, false},
		{65535, 0, true},  // wrap: 65535 precedes 0
		{0, 65535, false}, // wrap: 0 follows 65535
		{10, 10, false},
	}
	for _, c := range cases {
		if got := seqLess(c.a, c.b); got != c.want {
			t.Fatalf("seqLess(%d, %d) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
