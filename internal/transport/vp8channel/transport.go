package vp8channel

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/carrier"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	defaultMaxPayloadSize = 60 * 1024
	defaultConnectTimeout = 30 * time.Second
	dataMarker            = 0xFF
	rtpBufSize            = 65536
)

var (
	ErrVideoTrackUnsupported = errors.New("carrier does not support video tracks")
	ErrTransportClosed       = errors.New("vp8channel transport closed")
)

var vp8Keepalive = []byte{
	0x30, 0x01, 0x00, 0x9d, 0x01, 0x2a, 0x10, 0x00,
	0x10, 0x00, 0x00, 0x47, 0x08, 0x85, 0x85, 0x88,
	0x99, 0x84, 0x88, 0xfc,
}

type streamTransport struct {
	stream        carrier.VideoTrack
	track         *webrtc.TrackLocalStaticSample
	onData        func([]byte)
	outbound      chan []byte
	closeCh       chan struct{}
	writerDone    chan struct{}
	closed        atomic.Bool
	writerUp      atomic.Bool
	startOnce     sync.Once
	frameInterval time.Duration
	batchSize     int
}

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

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeVP8,
			ClockRate: 90000,
		},
		"vp8channel",
		"olcrtc",
	)
	if err != nil {
		return nil, fmt.Errorf("create local video track: %w", err)
	}

	fps := cfg.VP8FPS
	batchSize := cfg.VP8BatchSize

	tr := &streamTransport{
		stream:        stream,
		track:         track,
		onData:        cfg.OnData,
		outbound:      make(chan []byte, 256),
		closeCh:       make(chan struct{}),
		writerDone:    make(chan struct{}),
		frameInterval: time.Second / time.Duration(fps),
		batchSize:     batchSize,
	}

	if err := stream.AddTrack(track); err != nil {
		return nil, fmt.Errorf("attach local video track: %w", err)
	}
	stream.SetTrackHandler(tr.handleRemoteTrack)

	return tr, nil
}

func (p *streamTransport) Connect(ctx context.Context) error {
	connectCtx, cancel := context.WithTimeout(ctx, defaultConnectTimeout)
	defer cancel()

	if err := p.stream.Connect(connectCtx); err != nil {
		return err
	}

	p.startOnce.Do(func() {
		p.writerUp.Store(true)
		go p.writerLoop()
	})

	return nil
}

func (p *streamTransport) Send(data []byte) error {
	if p.closed.Load() {
		return ErrTransportClosed
	}

	frame := encodeDataFrame(data)

	select {
	case <-p.closeCh:
		return ErrTransportClosed
	case p.outbound <- frame:
		return nil
	}
}

func (p *streamTransport) Close() error {
	if p.closed.CompareAndSwap(false, true) {
		close(p.closeCh)
		if p.writerUp.Load() {
			<-p.writerDone
		}
		return p.stream.Close()
	}
	return nil
}

func (p *streamTransport) SetReconnectCallback(cb func()) {
	p.stream.SetReconnectCallback(cb)
}

func (p *streamTransport) SetShouldReconnect(fn func() bool) {
	p.stream.SetShouldReconnect(fn)
}

func (p *streamTransport) SetEndedCallback(cb func(string)) {
	p.stream.SetEndedCallback(cb)
}

func (p *streamTransport) WatchConnection(ctx context.Context) {
	p.stream.WatchConnection(ctx)
}

func (p *streamTransport) CanSend() bool {
	return !p.closed.Load() && p.stream.CanSend()
}

func (p *streamTransport) Features() transport.Features {
	return transport.Features{
		Reliable:        false,
		Ordered:         false,
		MessageOriented: true,
		MaxPayloadSize:  defaultMaxPayloadSize,
	}
}

func (p *streamTransport) writerLoop() {
	defer close(p.writerDone)

	ticker := time.NewTicker(p.frameInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.closeCh:
			return
		case <-ticker.C:
			sent := 0
			for sent < p.batchSize {
				var sample []byte
				select {
				case frame := <-p.outbound:
					sample = frame
				default:
					sample = vp8Keepalive
				}

				_ = p.track.WriteSample(media.Sample{
					Data:     sample,
					Duration: p.frameInterval,
				})
				sent++

				if sample[0] != dataMarker {
					break
				}
			}
		}
	}
}

func (p *streamTransport) handleRemoteTrack(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	if track.Codec().MimeType != webrtc.MimeTypeVP8 {
		go p.drainTrack(track)
		return
	}

	go p.readVP8Track(track)
}

func (p *streamTransport) drainTrack(track *webrtc.TrackRemote) {
	buf := make([]byte, rtpBufSize)
	for {
		if _, _, err := track.Read(buf); err != nil {
			return
		}
	}
}

func (p *streamTransport) readVP8Track(track *webrtc.TrackRemote) {
	var vp8Pkt codecs.VP8Packet
	var frameBuf []byte
	buf := make([]byte, rtpBufSize)

	for {
		n, _, err := track.Read(buf)
		if err != nil {
			return
		}

		pkt := &rtp.Packet{}
		if pkt.Unmarshal(buf[:n]) != nil {
			continue
		}

		vp8Payload, err := vp8Pkt.Unmarshal(pkt.Payload)
		if err != nil {
			continue
		}

		if vp8Pkt.S == 1 {
			frameBuf = frameBuf[:0]
		}
		frameBuf = append(frameBuf, vp8Payload...)

		if pkt.Marker {
			data := extractDataFromPayload(frameBuf)
			if data != nil && p.onData != nil {
				p.onData(data)
			}
		}
	}
}

func encodeDataFrame(data []byte) []byte {
	frame := make([]byte, 5+len(data))
	frame[0] = dataMarker
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(data)))
	copy(frame[5:], data)
	return frame
}

func extractDataFromPayload(frame []byte) []byte {
	if len(frame) < 5 || frame[0] != dataMarker {
		return nil
	}
	length := binary.BigEndian.Uint32(frame[1:5])
	if len(frame) < int(5+length) {
		return nil
	}
	return frame[5 : 5+length]
}
