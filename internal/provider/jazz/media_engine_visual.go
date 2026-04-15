// Package jazz implements the SaluteJazz WebRTC provider.
package jazz

import (
	"errors"
	"fmt"
	"image"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/provider"
	"github.com/openlibrecommunity/olcrtc/internal/visual/transport"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// visualEngine hosts the visual transport media path.
//
// The preferred runtime is the real VP8 path implemented behind the optional
// `vpx` build tag. The frame pump and frameReader/frameWriter workers below are
// intentionally kept only as a legacy fallback harness so the pure-Go visual
// transport can still be unit-tested without libvpx/toolchain availability.
// Other agents should not treat that fallback path as the target runtime
// architecture or extend it as if it were the long-term design.
type visualEngine struct {
	onPayload func([]byte)
	onClosed  func()

	sender   *transport.Sender
	receiver *transport.Receiver

	outerEncoder *transport.OuterFECEncoder
	outerDecoder *transport.OuterFECDecoder

	localTrack     *webrtc.TrackLocalStaticSample
	audioTrack     *webrtc.TrackLocalStaticSample
	rtpSender      *webrtc.RTPSender
	audioSender    *webrtc.RTPSender
	trackCID       string
	audioTrackCID  string
	audioAnnounced bool
	videoAnnounced bool
	audioPublished bool
	videoPublished bool
	readyCh        chan struct{}
	readyOnce      sync.Once

	// Legacy fallback/test harness fields. Keep for offline transport tests and
	// possible future experiments only. New production behaviour should prefer
	// the VP8-backed path instead of growing around these fields.
	ioMu        sync.RWMutex
	frameWriter visualFrameWriter
	frameReader visualFrameReader
	sampleCodec visualSampleCodec

	sendQ      chan []byte
	outboundQ  chan *image.YCbCr
	closeCh    chan struct{}
	closeOnce  sync.Once
	pumpOnce   sync.Once
	pumpClosed chan struct{}

	ioOnce     sync.Once
	writerDone chan struct{}
	readerDone chan struct{}

	metricsOnce sync.Once
	metricsDone chan struct{}

	logTrackOnce sync.Once
}

type visualFrameWriter interface {
	WriteFrame(frame *image.YCbCr) error
}

// visualFrameReader belongs to the legacy fallback scaffold. It remains useful
// for tests, but it is not the intended long-term live API for jazz-visual.
type visualFrameReader interface {
	ReadFrame() (*image.YCbCr, error)
}

type visualSampleCodec interface {
	EncodeFrame(frame *image.YCbCr) (media.Sample, error)
	Close() error
}

type visualSampleSink interface {
	WriteSample(sample media.Sample) error
}

var errNoVisualFrame = errors.New("no visual frame available")
var errVisualVP8Unavailable = errors.New("visual VP8 path unavailable")
var errAttachNilPC = errors.New("attach visual publisher track: nil peer connection")
var errAttachTracksNotInit = errors.New("attach visual publisher track: local tracks not initialised")

type visualTrackFrameWriter struct {
	sink  visualSampleSink
	codec visualSampleCodec
}

func newVisualTrackFrameWriter(
	sink visualSampleSink,
	codec visualSampleCodec,
) *visualTrackFrameWriter {
	return &visualTrackFrameWriter{sink: sink, codec: codec}
}

func (w *visualTrackFrameWriter) WriteFrame(frame *image.YCbCr) error {
	sample, err := w.codec.EncodeFrame(frame)
	if err != nil {
		return fmt.Errorf("encode visual frame to sample: %w", err)
	}
	if err := w.sink.WriteSample(sample); err != nil {
		return fmt.Errorf("write visual sample: %w", err)
	}
	return nil
}

func newVisualEngine() *visualEngine {
	return &visualEngine{
		sender:        transport.NewSender(40),
		receiver:      transport.NewReceiver(),
		outerEncoder:  transport.NewOuterFECEncoder(10, 4),
		outerDecoder:  transport.NewOuterFECDecoder(10, 4),
		trackCID:      uuid.NewString(),
		audioTrackCID: uuid.NewString(),
		readyCh:       make(chan struct{}),
		sendQ:         make(chan []byte, 5000),
		outboundQ:     make(chan *image.YCbCr, 5000),
		closeCh:       make(chan struct{}),
		pumpClosed:    make(chan struct{}),
		writerDone:    make(chan struct{}),
		readerDone:    make(chan struct{}),
		metricsDone:   make(chan struct{}),
	}
}

func (e *visualEngine) trackMetadata() map[string]any {
	return e.nextTrackMetadata()
}

func (e *visualEngine) setup(pcPub, pcSub *webrtc.PeerConnection) (<-chan struct{}, error) {
	pcPub.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		logger.Debugf("visual pcPub state=%s", state.String())
	})
	pcSub.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		logger.Debugf("visual pcSub state=%s", state.String())
	})

	if _, err := pcSub.AddTransceiverFromKind(
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	); err != nil {
		return nil, fmt.Errorf("add visual recvonly transceiver: %w", err)
	}

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeVP8,
			ClockRate: 90000,
		},
		e.trackCID,
		"-",
	)
	if err != nil {
		return nil, fmt.Errorf("create visual local track: %w", err)
	}

	audioTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		},
		e.audioTrackCID,
		"-",
	)
	if err != nil {
		return nil, fmt.Errorf("create muted audio local track: %w", err)
	}

	if err := e.installLiveVisualWriter(track); err != nil {
		return nil, fmt.Errorf("install live visual writer: %w", err)
	}

	pcSub.OnTrack(func(trackRemote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		logger.Debugf(
			"visual OnTrack candidate: kind=%s codec=%s id=%s stream=%s",
			trackRemote.Kind().String(),
			trackRemote.Codec().MimeType,
			trackRemote.ID(),
			trackRemote.StreamID(),
		)
		if trackRemote.Kind() != webrtc.RTPCodecTypeVideo {
			return
		}
		if !strings.EqualFold(trackRemote.Codec().MimeType, webrtc.MimeTypeVP8) {
			return
		}
		e.logTrackOnce.Do(func() {
			logger.Debugf(
				"visual OnTrack: codec=%s id=%s stream=%s",
				trackRemote.Codec().MimeType,
				trackRemote.ID(),
				trackRemote.StreamID(),
			)
		})
		if err := e.installLiveVisualReader(trackRemote); err != nil {
			if e.onClosed != nil {
				e.onClosed()
			}
			return
		}
		e.markReady()
	})

	e.localTrack = track
	e.audioTrack = audioTrack
	e.bindLocalTrackWriter(track)
	e.startFramePump()
	e.startSampleIO()
	e.startOuterFECMetrics()
	return e.readyCh, nil
}

func (e *visualEngine) attachPublisherTrack(pcPub *webrtc.PeerConnection) error {
	if pcPub == nil {
		return errAttachNilPC
	}

	e.ioMu.RLock()
	audioTrack := e.audioTrack
	track := e.localTrack
	audioSender := e.audioSender
	videoSender := e.rtpSender
	audioPublished := e.audioPublished
	videoPublished := e.videoPublished
	e.ioMu.RUnlock()

	if audioTrack == nil || track == nil {
		return errAttachTracksNotInit
	}

	if audioPublished && audioSender == nil {
		sender, err := pcPub.AddTrack(audioTrack)
		if err != nil {
			return fmt.Errorf("add muted audio local track: %w", err)
		}
		e.drainRTCP(sender)
		e.ioMu.Lock()
		e.audioSender = sender
		e.ioMu.Unlock()
	}

	if videoPublished && videoSender == nil {
		sender, err := pcPub.AddTrack(track)
		if err != nil {
			return fmt.Errorf("add visual local track: %w", err)
		}
		e.drainRTCP(sender)

		e.ioMu.Lock()
		e.rtpSender = sender
		e.ioMu.Unlock()
	}

	return nil
}

func (e *visualEngine) nextTrackMetadata() map[string]any {
	e.ioMu.Lock()
	defer e.ioMu.Unlock()

	if !e.audioAnnounced {
		e.audioAnnounced = true
		return map[string]any{
			"cid":    e.audioTrackCID,
			"type":   "AUDIO",
			"source": "MICROPHONE",
			"muted":  true,
		}
	}

	if !e.videoAnnounced {
		e.videoAnnounced = true
		return e.videoTrackMetadataLocked()
	}

	return nil
}

func (e *visualEngine) videoTrackMetadataLocked() map[string]any {
	const (
		highBitrate   = 180000
		mediumBitrate = 60000
		lowBitrate    = 20000
	)

	return map[string]any{
		"cid":    e.trackCID,
		"type":   "VIDEO",
		"source": "CAMERA",
		"muted":  false,
		"width":  640,
		"height": 480,
		"layers": []map[string]any{
			{
				"quality": "HIGH",
				"width":   640,
				"height":  480,
				"bitrate": highBitrate,
			},
			{
				"quality": "MEDIUM",
				"width":   320,
				"height":  240,
				"bitrate": mediumBitrate,
			},
			{
				"quality": "LOW",
				"width":   160,
				"height":  120,
				"bitrate": lowBitrate,
			},
		},
		"simulcastCodecs": []map[string]any{
			{
				"codec": "vp8",
				"cid":   e.trackCID,
			},
		},
	}
}

func (e *visualEngine) onTrackPublished(cid, _ string) {
	e.ioMu.Lock()
	defer e.ioMu.Unlock()

	switch cid {
	case e.audioTrackCID:
		e.audioPublished = true
	case e.trackCID:
		e.videoPublished = true
	}
}

func (e *visualEngine) publisherReady() bool {
	e.ioMu.RLock()
	defer e.ioMu.RUnlock()
	return e.videoPublished
}

func (e *visualEngine) drainRTCP(sender *webrtc.RTPSender) {
	if sender == nil {
		return
	}

	go func() {
		buf := make([]byte, 1500)
		for {
			if e.isClosed() {
				return
			}
			if _, _, err := sender.Read(buf); err != nil {
				if !e.isClosed() {
					logger.Debugf("visual sender RTCP read error: %v", err)
				}
				return
			}
		}
	}()
}

func (e *visualEngine) send(data []byte) error {
	if !e.isReady() {
		return provider.ErrDataChannelNotReady
	}

	return e.queueVisualPayload(data)
}

func (e *visualEngine) canSend() bool {
	if !e.isReady() || e.isClosed() {
		return false
	}
	return len(e.sendQ) < 4000
}

func (e *visualEngine) bufferedAmount() uint64 {
	return uint64(len(e.sendQ) + len(e.outboundQ))
}

func (e *visualEngine) sendQueue() chan []byte { return e.sendQ }

func (e *visualEngine) close() error {
	e.closeOnce.Do(func() {
		close(e.closeCh)
	})

	e.closeVisualResource(e.getSampleCodec())
	e.closeVisualResource(e.getFrameReader())
	e.closeVisualResource(e.getFrameWriter())

	e.waitForWorker(e.pumpClosed)
	e.waitForWorker(e.writerDone)
	e.waitForWorker(e.readerDone)
	e.waitForWorker(e.metricsDone)

	return nil
}

func (e *visualEngine) setOnPayload(cb func(payload []byte)) { e.onPayload = cb }

func (e *visualEngine) setOnClosed(cb func()) { e.onClosed = cb }

func (e *visualEngine) setFrameWriter(writer visualFrameWriter) {
	e.ioMu.Lock()
	defer e.ioMu.Unlock()
	e.frameWriter = writer
}

func (e *visualEngine) setFrameReader(reader visualFrameReader) {
	e.ioMu.Lock()
	defer e.ioMu.Unlock()
	e.frameReader = reader
}

func (e *visualEngine) setSampleCodec(codec visualSampleCodec) {
	e.ioMu.Lock()
	defer e.ioMu.Unlock()
	e.sampleCodec = codec
}

func (e *visualEngine) markReady() {
	e.readyOnce.Do(func() {
		close(e.readyCh)
	})
}

func (e *visualEngine) markPublisherReady() {
	logger.Debugf("visual publisher transport is ready")
	e.markReady()
}

// startFramePump turns queued application payloads into outbound visual frames.
//
// Each payload is first wrapped with an outer FEC group header. Every Nth
// payload also triggers M parity payloads which are encoded and queued
// immediately after, providing cross-payload RS redundancy.
//
// Legacy note: this queue-based path exists for fallback/testing. In a
// `-tags vpx` build the intended live implementation is the real VP8 encoder/
// decoder path, not further expansion of this scaffold.
func (e *visualEngine) startFramePump() {
	e.pumpOnce.Do(func() {
		go func() {
			defer close(e.pumpClosed)
			for {
				select {
				case <-e.closeCh:
					return
				case payload := <-e.sendQ:
					if err := e.pumpPayload(payload); err != nil {
						if e.onClosed != nil {
							e.onClosed()
						}
						return
					}
				}
			}
		}()
	})
}

// pumpPayload outer-FEC-wraps one application payload and encodes all resulting
// wrapped messages (data + any parity) into visual frames queued for the writer.
func (e *visualEngine) pumpPayload(payload []byte) error {
	wrapped, err := e.outerEncoder.Add(payload)
	if err != nil {
		return fmt.Errorf("outer FEC encode: %w", err)
	}
	for _, w := range wrapped {
		frames, err := e.sender.EncodeFrames(w)
		if err != nil {
			return fmt.Errorf("encode visual frames: %w", err)
		}
		for _, frame := range frames {
			select {
			case <-e.closeCh:
				return nil
			case e.outboundQ <- frame:
			}
		}
	}
	return nil
}

// startSampleIO launches the legacy frame-level workers once.
//
// Keep this path for tests/fallback builds. Do not add new production logic on
// top of these workers unless the visual transport design is explicitly being
// revised.
func (e *visualEngine) startSampleIO() {
	e.ioOnce.Do(func() {
		go e.runFrameWriter()
		go e.runFrameReader()
	})
}

func (e *visualEngine) runFrameWriter() {
	defer close(e.writerDone)
	for {
		writer := e.getFrameWriter()
		if writer == nil {
			if e.sleepOrClosed(10 * time.Millisecond) {
				return
			}
			continue
		}

		select {
		case <-e.closeCh:
			return
		case frame := <-e.outboundQ:
			if err := writer.WriteFrame(frame); err != nil {
				if e.onClosed != nil {
					e.onClosed()
				}
				return
			}
		}
	}
}

func (e *visualEngine) runFrameReader() {
	defer close(e.readerDone)
	for {
		reader, ok := e.waitForFrameReader()
		if !ok {
			return
		}
		if err := e.readOneFrame(reader); err != nil {
			if errors.Is(err, errNoVisualFrame) {
				if e.sleepOrClosed(10 * time.Millisecond) {
					return
				}
				continue
			}
			if e.onClosed != nil {
				e.onClosed()
			}
			return
		}
	}
}

// queueVisualPayload is the internal enqueue path used by send() and tests.
//
// This feeds the legacy frame pump. It stays valuable for offline transport
// tests, but should not be mistaken for the final end-to-end media path.
func (e *visualEngine) queueVisualPayload(payload []byte) error {
	select {
	case <-e.closeCh:
		return provider.ErrSendQueueClosed
	case e.sendQ <- payload:
		return nil
	default:
		return provider.ErrSendQueueTimeout
	}
}

// waitOutboundFrame blocks for up to timeout waiting for the next encoded
// outbound frame.
//
// Test-only legacy helper. Other agents should not use it as a production
// integration seam.
func (e *visualEngine) waitOutboundFrame(timeout time.Duration) (*image.YCbCr, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-e.closeCh:
		return nil, false
	case frame := <-e.outboundQ:
		return frame, true
	case <-timer.C:
		return nil, false
	}
}

// acceptDecodedFrame feeds one received visual frame into the transport
// receiver.  When the inner transport reassembles a complete payload, it is
// passed through the outer FEC decoder which delivers it (and any previously
// missing payloads recovered via RS parity) to the application callback.
//
// This remains useful for fallback tests and possible frame-domain
// experimentation, but the intended live inbound path is the VP8-backed reader.
func (e *visualEngine) acceptDecodedFrame(frame *image.YCbCr) error {
	payload, done, err := e.receiver.AcceptFrame(frame)
	if err != nil {
		return fmt.Errorf("accept decoded visual frame: %w", err)
	}
	if !done {
		return nil
	}
	payloads, err := e.outerDecoder.Accept(payload)
	if err != nil {
		return fmt.Errorf("outer FEC decode: %w", err)
	}
	for _, p := range payloads {
		if e.onPayload != nil {
			e.onPayload(p)
		}
	}
	return nil
}

func (e *visualEngine) startOuterFECMetrics() {
	e.metricsOnce.Do(func() {
		go e.runOuterFECMetrics()
	})
}

func (e *visualEngine) runOuterFECMetrics() {
	defer close(e.metricsDone)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-e.closeCh:
			return
		case <-ticker.C:
			s := e.outerDecoder.Stats()
			logger.Debugf(
				"outer FEC stats: lost=%d recovered=%d completed=%d abandoned=%d",
				s.FramesLost, s.FramesRecovered, s.GroupsCompleted, s.GroupsAbandoned,
			)
		}
	}
}

func (e *visualEngine) bindLocalTrackWriter(track visualSampleSink) {
	codec := e.getSampleCodec()
	if codec == nil || track == nil {
		return
	}
	e.setFrameWriter(newVisualTrackFrameWriter(track, codec))
}

func (e *visualEngine) installLiveVisualWriter(track *webrtc.TrackLocalStaticSample) error {
	codec, err := newVisualVP8SampleCodec()
	if err != nil {
		if errors.Is(err, errVisualVP8Unavailable) {
			return nil
		}
		return fmt.Errorf("create visual VP8 sample codec: %w", err)
	}
	e.setSampleCodec(codec)
	e.bindLocalTrackWriter(track)
	return nil
}

func (e *visualEngine) installLiveVisualReader(track *webrtc.TrackRemote) error {
	reader, err := newVisualVP8FrameReader(track)
	if err != nil {
		if errors.Is(err, errVisualVP8Unavailable) {
			return nil
		}
		return fmt.Errorf("create visual VP8 frame reader: %w", err)
	}
	e.setFrameReader(reader)
	return nil
}

func (e *visualEngine) getFrameWriter() visualFrameWriter {
	e.ioMu.RLock()
	defer e.ioMu.RUnlock()
	return e.frameWriter
}

func (e *visualEngine) getFrameReader() visualFrameReader {
	e.ioMu.RLock()
	defer e.ioMu.RUnlock()
	return e.frameReader
}

func (e *visualEngine) getSampleCodec() visualSampleCodec {
	e.ioMu.RLock()
	defer e.ioMu.RUnlock()
	return e.sampleCodec
}

func (e *visualEngine) isReady() bool {
	select {
	case <-e.readyCh:
		return true
	default:
		return false
	}
}

func (e *visualEngine) isClosed() bool {
	select {
	case <-e.closeCh:
		return true
	default:
		return false
	}
}

func (e *visualEngine) sleepOrClosed(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-e.closeCh:
		return true
	case <-timer.C:
		return false
	}
}

func (e *visualEngine) waitForWorker(done <-chan struct{}) {
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
	}
}

func (e *visualEngine) closeVisualResource(resource any) {
	closer, ok := resource.(io.Closer)
	if ok {
		_ = closer.Close()
		return
	}
	closeFn, ok := resource.(interface{ Close() error })
	if ok {
		_ = closeFn.Close()
	}
}

func (e *visualEngine) waitForFrameReader() (visualFrameReader, bool) {
	for {
		select {
		case <-e.closeCh:
			return nil, false
		default:
		}

		reader := e.getFrameReader()
		if reader != nil {
			return reader, true
		}
		if e.sleepOrClosed(10 * time.Millisecond) {
			return nil, false
		}
	}
}

func (e *visualEngine) readOneFrame(reader visualFrameReader) error {
	frame, err := reader.ReadFrame()
	if err != nil {
		return fmt.Errorf("read visual frame: %w", err)
	}
	if frame == nil {
		return errNoVisualFrame
	}
	if err := e.acceptDecodedFrame(frame); err != nil {
		return err
	}
	return nil
}
