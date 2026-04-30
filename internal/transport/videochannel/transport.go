// Package videochannel provides a byte transport over a visual video stream.
package videochannel

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/carrier"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

const (
	defaultMaxPayloadSize = 16 * 1024
	defaultFragmentSize   = 256
	defaultAckTimeout     = 1 * time.Second
	defaultFrameInterval  = 40 * time.Millisecond
	defaultConnectTimeout = 30 * time.Second
	maxSendAttempts       = 20
	sampleBuilderMaxLate  = 128
)

var (
	// ErrVideoTrackUnsupported is returned when a carrier cannot expose video tracks.
	ErrVideoTrackUnsupported = errors.New("carrier does not support video tracks")
	// ErrAckTimeout is returned when the peer does not acknowledge a payload in time.
	ErrAckTimeout = errors.New("videochannel ack timeout")
	// ErrTransportClosed is returned when operations are attempted on a closed transport.
	ErrTransportClosed = errors.New("videochannel transport closed")
)

type streamTransport struct {
	stream          carrier.VideoTrack
	track           *webrtc.TrackLocalStaticSample
	codec           codecSpec
	encoder         *ffmpegEncoder
	encoderMu       sync.Mutex
	decoder         *ffmpegDecoder
	decoderMu       sync.Mutex
	onData          func([]byte)
	outbound        chan []byte
	outboundAck     chan []byte
	closeCh         chan struct{}
	writerDone      chan struct{}
	nextSeq         atomic.Uint32
	closed          atomic.Bool
	writerUp        atomic.Bool
	sendMu          sync.Mutex
	startWriter     sync.Once
	ackMu           sync.Mutex
	ackWaiters      map[uint32]chan uint32
	recvMu          sync.Mutex
	inbound         map[uint32]*inboundMessage
	delivered       map[uint32]uint32
	videoW          int
	videoH          int
	videoFPS        int
	videoBitrate    string
	videoHW         string
	videoQRSize     int
	videoQRRecovery string
	videoCodec      string
	videoTileModule int
	videoTileRS     int

	// cached encoded idle frame — rendered and encoded once, reused on every tick
	// where the outbound queue is empty to avoid re-encoding an identical blank frame.
	idleFrame   []byte
	idleFrameMu sync.Mutex
}

// New creates a visual videochannel transport backed by a carrier-specific provider.
func New(ctx context.Context, cfg transport.Config) (transport.Transport, error) {
	session, err := carrier.New(ctx, cfg.Carrier, carrier.Config{
		RoomURL:   cfg.RoomURL,
		Name:      cfg.Name,
		OnData:    nil,
		DNSServer: cfg.DNSServer,
		ProxyAddr: cfg.ProxyAddr,
		ProxyPort: cfg.ProxyPort,
	})
	if err != nil {
		return nil, fmt.Errorf("create provider transport: %w", err)
	}

	videoCapable, ok := session.(carrier.VideoTrackCapable)
	if !ok {
		return nil, ErrVideoTrackUnsupported
	}

	stream, err := videoCapable.OpenVideoTrack()
	if err != nil {
		return nil, fmt.Errorf("open video track: %w", err)
	}

	codec := codecSpecForCarrier(cfg.Carrier)
	track, err := webrtc.NewTrackLocalStaticSample(codec.capability, "videochannel", "olcrtc")
	if err != nil {
		return nil, fmt.Errorf("create local video track: %w", err)
	}

	qrSize := cfg.VideoQRSize
	if qrSize <= 0 {
		qrSize = defaultFragmentSize
	}

	tileModule := cfg.VideoTileModule
	if tileModule <= 0 {
		tileModule = 4
	}

	tileRS := cfg.VideoTileRS
	if tileRS < 0 {
		tileRS = 20
	}

	tr := &streamTransport{
		stream:          stream,
		track:           track,
		codec:           codec,
		onData:          cfg.OnData,
		outbound:        make(chan []byte, 256),
		outboundAck:     make(chan []byte, 64),
		closeCh:         make(chan struct{}),
		writerDone:      make(chan struct{}),
		ackWaiters:      make(map[uint32]chan uint32),
		inbound:         make(map[uint32]*inboundMessage),
		delivered:       make(map[uint32]uint32),
		videoW:          cfg.VideoWidth,
		videoH:          cfg.VideoHeight,
		videoFPS:        cfg.VideoFPS,
		videoBitrate:    cfg.VideoBitrate,
		videoHW:         cfg.VideoHW,
		videoQRSize:     qrSize,
		videoQRRecovery: cfg.VideoQRRecovery,
		videoCodec:      cfg.VideoCodec,
		videoTileModule: tileModule,
		videoTileRS:     tileRS,
	}

	if err := stream.AddTrack(track); err != nil {
		return nil, fmt.Errorf("attach local video track: %w", err)
	}
	stream.SetTrackHandler(tr.handleRemoteTrack)

	return tr, nil
}

// Connect starts the transport connection.
func (p *streamTransport) Connect(ctx context.Context) error {
	connectCtx, cancel := context.WithTimeout(ctx, defaultConnectTimeout)
	defer cancel()

	encoder, err := newFFmpegEncoder(p.codec, p.videoW, p.videoH, p.videoFPS, p.videoBitrate, p.videoHW)
	if err != nil {
		return err
	}

	if err := p.stream.Connect(connectCtx); err != nil {
		_ = encoder.Close()
		return err
	}

	p.encoderMu.Lock()
	if p.closed.Load() {
		p.encoderMu.Unlock()
		_ = encoder.Close()
		return ErrTransportClosed
	}
	if p.encoder != nil {
		_ = p.encoder.Close()
	}
	p.encoder = encoder
	p.encoderMu.Unlock()

	p.startWriter.Do(func() {
		p.writerUp.Store(true)
		go p.writerLoop()
	})

	return nil
}

// Send transmits data through the transport.
func (p *streamTransport) Send(data []byte) error {
	if p.closed.Load() {
		return ErrTransportClosed
	}

	p.sendMu.Lock()
	defer p.sendMu.Unlock()

	seq := p.nextSeq.Add(1)
	crc := crc32.ChecksumIEEE(data)
	fragments := fragmentPayload(data, p.videoQRSize)
	waiter := make(chan uint32, 1)

	p.ackMu.Lock()
	p.ackWaiters[seq] = waiter
	p.ackMu.Unlock()
	defer func() {
		p.ackMu.Lock()
		delete(p.ackWaiters, seq)
		p.ackMu.Unlock()
	}()

	for attempt := 0; attempt < maxSendAttempts; attempt++ {
		for idx, fragment := range fragments {
			frame := encodeDataFrame(seq, crc, len(data), idx, len(fragments), fragment)
			if err := p.enqueueFrame(frame, false); err != nil {
				return err
			}
		}

		timer := time.NewTimer(defaultAckTimeout)
		select {
		case ackCRC := <-waiter:
			timer.Stop()
			if ackCRC == crc {
				return nil
			}
		case <-timer.C:
		case <-p.closeCh:
			timer.Stop()
			return ErrTransportClosed
		}
	}

	return ErrAckTimeout
}

// Close terminates the transport.
func (p *streamTransport) Close() error {
	if p.closed.CompareAndSwap(false, true) {
		close(p.closeCh)

		p.encoderMu.Lock()
		if p.encoder != nil {
			_ = p.encoder.Close()
		}
		p.encoderMu.Unlock()

		p.decoderMu.Lock()
		if p.decoder != nil {
			_ = p.decoder.Close()
		}
		p.decoderMu.Unlock()

		if p.writerUp.Load() {
			<-p.writerDone
		}
		return p.stream.Close()
	}
	return nil
}

// SetReconnectCallback registers reconnect handling.
func (p *streamTransport) SetReconnectCallback(cb func()) {
	p.stream.SetReconnectCallback(cb)
}

// SetShouldReconnect configures reconnect policy.
func (p *streamTransport) SetShouldReconnect(fn func() bool) {
	p.stream.SetShouldReconnect(fn)
}

// SetEndedCallback registers end-of-session handling.
func (p *streamTransport) SetEndedCallback(cb func(string)) {
	p.stream.SetEndedCallback(cb)
}

// WatchConnection monitors connection lifecycle.
func (p *streamTransport) WatchConnection(ctx context.Context) {
	p.stream.WatchConnection(ctx)
}

// CanSend reports whether transport is ready for sending.
func (p *streamTransport) CanSend() bool {
	return !p.closed.Load() && p.stream.CanSend()
}

// Features describes the current videochannel transport semantics.
func (p *streamTransport) Features() transport.Features {
	maxPayload := defaultMaxPayloadSize
	if p.videoQRSize*64 > maxPayload {
		maxPayload = p.videoQRSize * 64
	}
	return transport.Features{
		Reliable:        true,
		Ordered:         true,
		MessageOriented: true,
		MaxPayloadSize:  maxPayload,
	}
}

func (p *streamTransport) writerLoop() {
	defer close(p.writerDone)
	defer func() {
		p.encoderMu.Lock()
		defer p.encoderMu.Unlock()
		if p.encoder != nil {
			_ = p.encoder.Close()
		}
	}()

	ticker := time.NewTicker(time.Second / time.Duration(p.videoFPS))
	defer ticker.Stop()

	frameDuration := time.Second / time.Duration(p.videoFPS)

	for {
		select {
		case <-p.closeCh:
			return
		case <-ticker.C:
			payload, ok := p.nextOutboundFrame()
			if !ok {
				return
			}

			p.encoderMu.Lock()
			enc := p.encoder
			p.encoderMu.Unlock()

			if enc == nil {
				continue
			}

			// idle frame: payload is nil — reuse previously encoded sample to avoid
			// re-rendering and re-encoding an identical blank frame every tick.
			if payload == nil {
				p.idleFrameMu.Lock()
				cached := p.idleFrame
				p.idleFrameMu.Unlock()

				if cached == nil {
					// first time — render + encode once, then cache
					rawFrame, err := renderVisualFrame(nil, p.videoW, p.videoH, p.videoCodec, p.videoQRRecovery, p.videoTileModule, p.videoTileRS)
					if err != nil {
						logger.Debugf("videochannel render idle error: %v", err)
						continue
					}
					sample, err := enc.EncodeFrame(rawFrame)
					if err != nil {
						logger.Warnf("videochannel encoder idle error: %v", err)
						continue
					}
					p.idleFrameMu.Lock()
					p.idleFrame = sample
					p.idleFrameMu.Unlock()
					cached = sample
				}

				_ = p.track.WriteSample(media.Sample{
					Data:     cached,
					Duration: frameDuration,
				})
				continue
			}

			rawFrame, err := renderVisualFrame(payload, p.videoW, p.videoH, p.videoCodec, p.videoQRRecovery, p.videoTileModule, p.videoTileRS)
			if err != nil {
				logger.Debugf("videochannel render error: %v", err)
				continue
			}

			sample, err := enc.EncodeFrame(rawFrame)
			if err != nil {
				logger.Warnf("videochannel encoder error: %v", err)
				continue
			}

			_ = p.track.WriteSample(media.Sample{
				Data:     sample,
				Duration: frameDuration,
			})
		}
	}
}

func (p *streamTransport) nextOutboundFrame() ([]byte, bool) {
	select {
	case <-p.closeCh:
		return nil, false
	case payload := <-p.outboundAck:
		return payload, true
	default:
	}

	select {
	case <-p.closeCh:
		return nil, false
	case payload := <-p.outboundAck:
		return payload, true
	case payload := <-p.outbound:
		return payload, true
	default:
		return nil, true
	}
}

func (p *streamTransport) enqueueFrame(frame []byte, priority bool) error {
	if p.closed.Load() {
		return ErrTransportClosed
	}

	ch := p.outbound
	if priority {
		ch = p.outboundAck
	}

	select {
	case <-p.closeCh:
		return ErrTransportClosed
	case ch <- frame:
		return nil
	}
}

func (p *streamTransport) handleRemoteTrack(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	codec, ok := codecSpecForMime(track.Codec().MimeType)
	if !ok {
		logger.Warnf("videochannel unsupported remote codec: %s", track.Codec().MimeType)
		return
	}

	decoder, err := newFFmpegDecoder(codec, p.videoW, p.videoH, p.videoFPS, p.videoHW)
	if err != nil {
		logger.Warnf("videochannel decoder init failed: %v", err)
		return
	}

	p.decoderMu.Lock()
	if p.closed.Load() {
		p.decoderMu.Unlock()
		_ = decoder.Close()
		return
	}
	if p.decoder != nil {
		_ = p.decoder.Close()
	}
	p.decoder = decoder
	p.decoderMu.Unlock()

	go func() {
		defer func() {
			p.decoderMu.Lock()
			if p.decoder == decoder {
				p.decoder = nil
			}
			p.decoderMu.Unlock()
			_ = decoder.Close()
		}()

		for {
			select {
			case <-p.closeCh:
				return
			default:
			}

			frame, err := decoder.PopFrame()
			if err != nil {
				if !errors.Is(err, ErrTransportClosed) && !p.closed.Load() {
					logger.Warnf("videochannel decoder pop error: %v", err)
				}
				return
			}
			p.handleFrame(frame)
		}
	}()

	go func() {
		sb := samplebuilder.New(sampleBuilderMaxLate, codec.depacketizer(), track.Codec().ClockRate)
		for {
			select {
			case <-p.closeCh:
				return
			default:
			}

			packet, _, err := track.ReadRTP()
			if err != nil {
				sb.Flush()
				return
			}

			sb.Push(packet)
			for sample := sb.Pop(); sample != nil; sample = sb.Pop() {
				if err := decoder.PushSample(sample.Data); err != nil {
					if !p.closed.Load() {
						logger.Warnf("videochannel decoder push error: %v", err)
					}
					return
				}
			}
		}
	}()
}

func (p *streamTransport) handleFrame(frame []byte) {
	var payload []byte
	var err error
	payload, err = extractVisualPayload(frame, p.videoW, p.videoH, p.videoCodec, p.videoTileModule, p.videoTileRS)
	if err != nil || len(payload) == 0 {
		if err != nil {
			logger.Debugf("videochannel extract visual payload error: %v", err)
		}
		return
	}

	logger.Debugf("videochannel extracted visual payload: len=%d", len(payload))

	decoded, err := decodeTransportFrame(payload)
	if err != nil {
		logger.Debugf("videochannel decode transport frame error: %v", err)
		return
	}

	logger.Debugf("videochannel transport frame: type=%d seq=%d crc=%x", decoded.typ, decoded.seq, decoded.crc)

	switch decoded.typ {
	case frameTypeAck:
		p.resolveAck(decoded.seq, decoded.crc)
	case frameTypeData:
		p.handleInboundFrame(decoded)
	}
}

func (p *streamTransport) handleInboundFrame(frame transportFrame) {
	p.recvMu.Lock()
	if crc, ok := p.delivered[frame.seq]; ok && crc == frame.crc {
		p.recvMu.Unlock()
		p.sendAck(frame.seq, frame.crc)
		return
	}

	msg, ok := p.inbound[frame.seq]
	if !ok || msg.crc != frame.crc || msg.totalLen != frame.totalLen || len(msg.frags) != int(frame.fragTotal) {
		msg = &inboundMessage{
			totalLen: frame.totalLen,
			crc:      frame.crc,
			frags:    make([][]byte, frame.fragTotal),
			remain:   int(frame.fragTotal),
		}
		p.inbound[frame.seq] = msg
	}

	if int(frame.fragIdx) >= len(msg.frags) {
		p.recvMu.Unlock()
		return
	}

	if msg.frags[frame.fragIdx] == nil {
		chunk := make([]byte, len(frame.payload))
		copy(chunk, frame.payload)
		msg.frags[frame.fragIdx] = chunk
		msg.remain--
	}

	if msg.remain > 0 {
		p.recvMu.Unlock()
		return
	}

	delete(p.inbound, frame.seq)
	data := make([]byte, 0, msg.totalLen)
	for _, frag := range msg.frags {
		data = append(data, frag...)
	}

	if uint32(len(data)) > msg.totalLen {
		data = data[:msg.totalLen]
	}

	if crc32.ChecksumIEEE(data) != msg.crc {
		p.recvMu.Unlock()
		return
	}

	if len(p.delivered) > 256 {
		p.delivered = make(map[uint32]uint32)
	}
	p.delivered[frame.seq] = msg.crc
	p.recvMu.Unlock()

	if p.onData != nil {
		p.onData(data)
	}
	p.sendAck(frame.seq, frame.crc)
}

func (p *streamTransport) sendAck(seq, crc uint32) {
	_ = p.enqueueFrame(encodeAckFrame(seq, crc), true)
}

func (p *streamTransport) resolveAck(seq, crc uint32) {
	p.ackMu.Lock()
	waiter := p.ackWaiters[seq]
	p.ackMu.Unlock()

	if waiter == nil {
		return
	}

	select {
	case waiter <- crc:
	default:
	}
}
