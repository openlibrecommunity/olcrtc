package jazz

import (
	"bytes"
	"errors"
	"image"
	"sync"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/provider"
	"github.com/pion/webrtc/v4/pkg/media"
)

type captureFrameWriter struct {
	mu     sync.Mutex
	frames []*image.YCbCr
}

func (w *captureFrameWriter) WriteFrame(frame *image.YCbCr) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.frames = append(w.frames, frame)
	return nil
}

func (w *captureFrameWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.frames)
}

type queueFrameReader struct {
	ch chan *image.YCbCr
}

func (r *queueFrameReader) ReadFrame() (*image.YCbCr, error) {
	select {
	case frame := <-r.ch:
		return frame, nil
	default:
		return nil, errNoVisualFrame
	}
}

type captureSampleSink struct {
	mu      sync.Mutex
	samples []media.Sample
}

func (s *captureSampleSink) WriteSample(sample media.Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, sample)
	return nil
}

func (s *captureSampleSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.samples)
}

func (s *captureSampleSink) first() media.Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.samples[0]
}

type stubVisualSampleCodec struct {
	sample media.Sample
	err    error
}

func (c stubVisualSampleCodec) EncodeFrame(_ *image.YCbCr) (media.Sample, error) {
	if c.err != nil {
		return media.Sample{}, c.err
	}
	return c.sample, nil
}

func (c stubVisualSampleCodec) Close() error {
	return nil
}

func TestVisualEngineFrameWriterWorker(t *testing.T) {
	engine := newVisualEngine()
	writer := &captureFrameWriter{}
	engine.setFrameWriter(writer)
	engine.startSampleIO()

	frame := image.NewYCbCr(image.Rect(0, 0, 16, 16), image.YCbCrSubsampleRatio420)
	select {
	case engine.outboundQ <- frame:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("failed to queue outbound frame")
	}

	deadline := time.Now().Add(200 * time.Millisecond)
	for writer.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if writer.count() != 1 {
		t.Fatalf("writer frame count = %d, want 1", writer.count())
	}
}

func TestVisualEngineFrameReaderWorker(t *testing.T) {
	engine := newVisualEngine()
	reader := &queueFrameReader{ch: make(chan *image.YCbCr, 8)}
	engine.setFrameReader(reader)

	var got []byte
	engine.setOnPayload(func(payload []byte) {
		got = append([]byte(nil), payload...)
	})
	engine.startSampleIO()

	src := newVisualEngine()
	src.startFramePump()
	payload := makeVisualPayload(800, 21)
	if err := src.queueVisualPayload(payload); err != nil {
		t.Fatalf("queueVisualPayload: %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for len(got) == 0 && time.Now().Before(deadline) {
		frame, ok := src.waitOutboundFrame(50 * time.Millisecond)
		if !ok {
			continue
		}
		reader.ch <- frame
		time.Sleep(10 * time.Millisecond)
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch:\n got  %x\n want %x", got, payload)
	}
}

func TestVisualEngineSendRequiresReady(t *testing.T) {
	engine := newVisualEngine()

	err := engine.send([]byte("hello"))
	if !errors.Is(err, provider.ErrDataChannelNotReady) {
		t.Fatalf("send error = %v, want %v", err, provider.ErrDataChannelNotReady)
	}
}

func TestVisualEngineSendQueuesPayloadWhenReady(t *testing.T) {
	engine := newVisualEngine()
	engine.markReady()

	payload := []byte("hello visual")
	if err := engine.send(payload); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case got := <-engine.sendQ:
		if !bytes.Equal(got, payload) {
			t.Fatalf("queued payload = %q, want %q", got, payload)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for queued payload")
	}
}

func TestVisualEngineFrameWriterWaitsForWriter(t *testing.T) {
	engine := newVisualEngine()
	engine.startSampleIO()

	frame := image.NewYCbCr(image.Rect(0, 0, 16, 16), image.YCbCrSubsampleRatio420)
	select {
	case engine.outboundQ <- frame:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("failed to queue outbound frame")
	}

	time.Sleep(30 * time.Millisecond)
	if got := len(engine.outboundQ); got != 1 {
		t.Fatalf("outbound queue length = %d, want 1 while writer is absent", got)
	}

	writer := &captureFrameWriter{}
	engine.setFrameWriter(writer)

	deadline := time.Now().Add(200 * time.Millisecond)
	for writer.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if writer.count() != 1 {
		t.Fatalf("writer frame count = %d, want 1", writer.count())
	}
}

func TestVisualTrackFrameWriterWritesEncodedSample(t *testing.T) {
	sink := &captureSampleSink{}
	want := media.Sample{
		Data:     []byte{0x01, 0x02, 0x03},
		Duration: 33 * time.Millisecond,
	}
	writer := newVisualTrackFrameWriter(sink, stubVisualSampleCodec{sample: want})

	frame := image.NewYCbCr(image.Rect(0, 0, 16, 16), image.YCbCrSubsampleRatio420)
	if err := writer.WriteFrame(frame); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if sink.count() != 1 {
		t.Fatalf("sample count = %d, want 1", sink.count())
	}
	got := sink.first()
	if !bytes.Equal(got.Data, want.Data) {
		t.Fatalf("sample data = %x, want %x", got.Data, want.Data)
	}
	if got.Duration != want.Duration {
		t.Fatalf("sample duration = %v, want %v", got.Duration, want.Duration)
	}
}

func TestVisualEngineBindLocalTrackWriterRequiresSampleCodec(t *testing.T) {
	engine := newVisualEngine()
	sink := &captureSampleSink{}

	engine.bindLocalTrackWriter(sink)
	if got := engine.getFrameWriter(); got != nil {
		t.Fatal("frame writer installed without sample codec")
	}

	engine.setSampleCodec(stubVisualSampleCodec{sample: media.Sample{Data: []byte{0xAA}}})
	engine.bindLocalTrackWriter(sink)
	if got := engine.getFrameWriter(); got == nil {
		t.Fatal("frame writer not installed after sample codec binding")
	}
}
