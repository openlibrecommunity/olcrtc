//go:build vpx && cgo

package jazz

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"
	"sync"
	"time"

	mdcodec "github.com/pion/mediadevices/pkg/codec"
	"github.com/pion/mediadevices/pkg/codec/vpx"
	"github.com/pion/mediadevices/pkg/frame"
	"github.com/pion/mediadevices/pkg/io/video"
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	visualcodec "github.com/openlibrecommunity/olcrtc/internal/visual/codec"
)

const (
	visualVP8FrameRate        = 15
	visualVP8TargetBitrateBps = 180_000
	visualVP8MaxLatePackets   = 128
)

var errDecodedVisualFrameType = errors.New("decoded image is not *image.YCbCr")

// visualVP8SampleCodec is the preferred live-path implementation for visual
// transport when built with `-tags vpx`. Future transport work should prefer
// extending this path over the legacy fallback scaffold in
// media_engine_visual.go.
type visualVP8SampleCodec struct {
	mu       sync.Mutex
	encoder  mdcodec.ReadCloser
	inputCh  chan *image.YCbCr
	closeCh  chan struct{}
	closeErr error
}

func newVisualVP8SampleCodec() (visualSampleCodec, error) {
	params, err := vpx.NewVP8Params()
	if err != nil {
		return nil, fmt.Errorf("new VP8 params: %w", err)
	}
	params.BitRate = visualVP8TargetBitrateBps
	params.KeyFrameInterval = 1
	params.LagInFrames = 0

	inputCh := make(chan *image.YCbCr, 1)
	closeCh := make(chan struct{})

	encoder, err := params.BuildVideoEncoder(
		video.ReaderFunc(func() (image.Image, func(), error) {
			select {
			case <-closeCh:
				return nil, nil, io.EOF
			case frame := <-inputCh:
				return frame, func() {}, nil
			}
		}),
		prop.Media{
			Video: prop.Video{
				Width:       visualcodec.FrameWidth,
				Height:      visualcodec.FrameHeight,
				FrameRate:   visualVP8FrameRate,
				FrameFormat: frame.FormatI420,
			},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("build VP8 encoder: %w", err)
	}

	return &visualVP8SampleCodec{
		encoder: encoder,
		inputCh: inputCh,
		closeCh: closeCh,
	}, nil
}

func (c *visualVP8SampleCodec) EncodeFrame(frame *image.YCbCr) (media.Sample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	select {
	case <-c.closeCh:
		return media.Sample{}, io.EOF
	case c.inputCh <- frame:
	}

	data, release, err := c.encoder.Read()
	if err != nil {
		return media.Sample{}, fmt.Errorf("read encoded VP8 frame: %w", err)
	}
	defer release()

	return media.Sample{
		Data:     bytes.Clone(data),
		Duration: time.Second / visualVP8FrameRate,
	}, nil
}

func (c *visualVP8SampleCodec) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	select {
	case <-c.closeCh:
	default:
		close(c.closeCh)
	}

	if c.encoder != nil && c.closeErr == nil {
		c.closeErr = c.encoder.Close()
	}
	return c.closeErr
}

type visualVP8FrameReader struct {
	decoder mdcodec.VideoDecoder
	pr      *io.PipeReader
	pw      *io.PipeWriter
	closeCh chan struct{}
	closeMu sync.Once
}

func newVisualVP8FrameReader(track *webrtc.TrackRemote) (visualFrameReader, error) {
	pr, pw := io.Pipe()
	decoder, err := vpx.BuildVideoDecoder(
		pr,
		prop.Media{
			Video: prop.Video{
				Width:       visualcodec.FrameWidth,
				Height:      visualcodec.FrameHeight,
				FrameFormat: frame.FormatI420,
			},
		},
	)
	if err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, fmt.Errorf("build VP8 decoder: %w", err)
	}

	reader := &visualVP8FrameReader{
		decoder: decoder,
		pr:      pr,
		pw:      pw,
		closeCh: make(chan struct{}),
	}
	go reader.pumpTrack(track)
	return reader, nil
}

func (r *visualVP8FrameReader) ReadFrame() (*image.YCbCr, error) {
	img, release, err := r.decoder.Read()
	if err != nil {
		return nil, fmt.Errorf("decode VP8 sample: %w", err)
	}
	defer release()

	frame, ok := img.(*image.YCbCr)
	if !ok {
		return nil, fmt.Errorf("%w: %T", errDecodedVisualFrameType, img)
	}

	return cloneYCbCr(frame), nil
}

func (r *visualVP8FrameReader) Close() error {
	r.closeMu.Do(func() {
		close(r.closeCh)
		_ = r.pr.Close()
		_ = r.pw.Close()
		_ = r.decoder.Close()
	})
	return nil
}

func (r *visualVP8FrameReader) pumpTrack(track *webrtc.TrackRemote) {
	builder := samplebuilder.New(
		visualVP8MaxLatePackets,
		&codecs.VP8Packet{},
		track.Codec().ClockRate,
	)

	for {
		select {
		case <-r.closeCh:
			return
		default:
		}

		packet, _, err := track.ReadRTP()
		if err != nil {
			_ = r.pw.CloseWithError(err)
			return
		}

		builder.Push(packet)
		for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
			if _, err := r.pw.Write(sample.Data); err != nil {
				return
			}
		}
	}
}

func cloneYCbCr(src *image.YCbCr) *image.YCbCr {
	dst := image.NewYCbCr(src.Rect, src.SubsampleRatio)
	copy(dst.Y, src.Y)
	copy(dst.Cb, src.Cb)
	copy(dst.Cr, src.Cr)
	return dst
}
