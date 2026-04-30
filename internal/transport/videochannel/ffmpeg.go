package videochannel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
)

const (
	ffmpegFrameTimeout = 10 * time.Second
)

var (
	// ErrFFmpegUnavailable is returned when ffmpeg is not available on PATH.
	ErrFFmpegUnavailable = errors.New("ffmpeg is required for videochannel")
	// ErrUnsupportedVideoCodec is returned when videochannel cannot decode the negotiated codec.
	ErrUnsupportedVideoCodec = errors.New("unsupported video codec")
)

type codecSpec struct {
	mimeType     string
	fourCC       string
	encoder      string
	capability   webrtc.RTPCodecCapability
	depacketizer func() rtp.Depacketizer
	encodeArgs   []string
}

func codecSpecForCarrier(carrier string) codecSpec {
	// Natural default for most WebRTC providers
	return vp8CodecSpec()
}

func codecSpecForMime(mimeType string) (codecSpec, bool) {
	switch strings.ToLower(mimeType) {
	case strings.ToLower(webrtc.MimeTypeH264):
		return h264CodecSpec(), true
	case strings.ToLower(webrtc.MimeTypeVP9):
		return vp9CodecSpec(), true
	case strings.ToLower(webrtc.MimeTypeVP8):
		return vp8CodecSpec(), true
	default:
		return codecSpec{}, false
	}
}

func h264CodecSpec() codecSpec {
	return codecSpec{
		mimeType: webrtc.MimeTypeH264,
		fourCC:   "H264",
		encoder:  "libx264",
		capability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeH264,
			ClockRate: 90000,
		},
		depacketizer: func() rtp.Depacketizer { return &codecs.H264Packet{} },
		encodeArgs: []string{
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-tune", "zerolatency",
			"-g", "1",
			"-pix_fmt", "yuv420p",
		},
	}
}

func vp9CodecSpec() codecSpec {
	return codecSpec{
		mimeType: webrtc.MimeTypeVP9,
		fourCC:   "VP90",
		encoder:  "libvpx-vp9",
		capability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeVP9,
			ClockRate: 90000,
		},
		depacketizer: func() rtp.Depacketizer { return &codecs.VP9Packet{} },
		encodeArgs: []string{
			"-c:v", "libvpx-vp9",
			"-deadline", "realtime",
			"-cpu-used", "8",
			"-error-resilient", "1",
			"-static-thresh", "0",
			"-g", "1",
			"-pix_fmt", "yuv420p",
		},
	}
}

func vp8CodecSpec() codecSpec {
	return codecSpec{
		mimeType: webrtc.MimeTypeVP8,
		fourCC:   "VP80",
		encoder:  "libvpx",
		capability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeVP8,
			ClockRate: 90000,
		},
		depacketizer: func() rtp.Depacketizer { return &codecs.VP8Packet{} },
		encodeArgs: []string{
			"-c:v", "libvpx",
			"-deadline", "realtime",
			"-cpu-used", "8",
			"-error-resilient", "1",
			"-static-thresh", "0",
			"-g", "1",
			"-pix_fmt", "yuv420p",
		},
	}
}

type ffmpegEncoder struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stderr    *bytes.Buffer
	frames    chan []byte
	width     int
	height    int
	frameSize int
	closed    atomic.Bool
	closeOnce sync.Once
	errMu     sync.Mutex
	err       error
}

func newFFmpegEncoder(spec codecSpec, width, height, fps int, bitrate, hw string) (*ffmpegEncoder, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, ErrFFmpegUnavailable
	}

	args := []string{"-loglevel", "error", "-threads", "1"}

	// Determine encoder binary based on HW flag
	vcodec := spec.encoder
	if hw == "nvenc" {
		switch spec.mimeType {
		case webrtc.MimeTypeH264:
			vcodec = "h264_nvenc"
		case webrtc.MimeTypeVP8:
			vcodec = "vp8_nvenc"
		case webrtc.MimeTypeVP9:
			vcodec = "vp9_nvenc"
		case webrtc.MimeTypeAV1:
			vcodec = "av1_nvenc"
		}
	}

	inputPixFmt := "gray"
	frameSize := width * height

	args = append(args,
		"-f", "rawvideo",
		"-pix_fmt", inputPixFmt,
		"-video_size", fmt.Sprintf("%dx%d", width, height),
		"-framerate", fmt.Sprintf("%d", fps),
		"-i", "pipe:0",
		"-an",
	)

	// Apply hardware specific flags if using NVENC
	if strings.HasSuffix(vcodec, "_nvenc") {
		args = append(args,
			"-c:v", vcodec,
			"-preset", "p1",
			"-tune", "ull",
			"-rc", "vbr",
		)
	} else {
		// Use software encoder args from spec
		args = append(args, spec.encodeArgs...)
	}

	args = append(args, "-g", "1", "-pix_fmt", "yuv420p", "-b:v", bitrate)

	if spec.mimeType == webrtc.MimeTypeH264 {
		args = append(args, "-f", "h264", "pipe:1")
	} else {
		args = append(args, "-f", "ivf", "pipe:1")
	}

	cmd := exec.Command("ffmpeg", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("encoder stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("encoder stdout: %w", err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start encoder: %w", err)
	}

	enc := &ffmpegEncoder{
		cmd:       cmd,
		stdin:     stdin,
		stderr:    stderr,
		frames:    make(chan []byte, 8),
		width:     width,
		height:    height,
		frameSize: frameSize,
	}

	if spec.mimeType == webrtc.MimeTypeH264 {
		go enc.readRawH264(stdout)
	} else {
		go enc.readIVF(stdout)
	}
	return enc, nil
}

func (e *ffmpegEncoder) EncodeFrame(frame []byte) ([]byte, error) {
	if len(frame) != e.frameSize {
		return nil, fmt.Errorf("unexpected encoder frame size: %d (expected %d)", len(frame), e.frameSize)
	}
	if err := e.processErr(); err != nil {
		return nil, err
	}
	if err := writeAll(e.stdin, frame); err != nil {
		return nil, fmt.Errorf("write encoder frame: %w", err)
	}

	select {
	case sample, ok := <-e.frames:
		if !ok {
			return nil, e.processErr()
		}
		return sample, nil
	case <-time.After(ffmpegFrameTimeout):
		if err := e.processErr(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("encoder timeout")
	}
}

func (e *ffmpegEncoder) Close() error {
	e.closeOnce.Do(func() {
		e.closed.Store(true)
		_ = e.stdin.Close()
		if e.cmd.Process != nil {
			_ = e.cmd.Process.Kill()
		}
		_ = e.cmd.Wait()
	})
	return nil
}

func (e *ffmpegEncoder) readIVF(stdout io.Reader) {
	defer close(e.frames)
	reader, _, err := ivfreader.NewWith(stdout)
	if err != nil {
		e.setErr(fmt.Errorf("encoder ivf header: %w", err))
		return
	}
	for {
		frame, _, err := reader.ParseNextFrame()
		if err != nil {
			if !e.closed.Load() {
				e.setErr(fmt.Errorf("encoder ivf read: %w", err))
			}
			return
		}
		copyFrame := append([]byte(nil), frame...)
		if e.closed.Load() {
			return
		}
		e.frames <- copyFrame
	}
}

func (e *ffmpegEncoder) readRawH264(stdout io.Reader) {
	defer close(e.frames)
	buf := make([]byte, 1024*1024)
	for {
		n, err := stdout.Read(buf)
		if err != nil {
			if !e.closed.Load() {
				e.setErr(fmt.Errorf("encoder h264 read: %w", err))
			}
			return
		}
		if n > 0 {
			copyFrame := append([]byte(nil), buf[:n]...)
			if e.closed.Load() {
				return
			}
			e.frames <- copyFrame
		}
	}
}

func (e *ffmpegEncoder) setErr(err error) {
	if err == nil {
		return
	}
	e.errMu.Lock()
	defer e.errMu.Unlock()
	if e.err == nil {
		e.err = withStderr(err, e.stderr)
	}
}

func (e *ffmpegEncoder) processErr() error {
	e.errMu.Lock()
	defer e.errMu.Unlock()
	if e.err != nil {
		return e.err
	}
	if e.closed.Load() {
		return ErrTransportClosed
	}
	return nil
}

type ffmpegDecoder struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stderr    *bytes.Buffer
	frames    chan []byte
	pts       uint64
	mimeType  string
	frameSize int
	closed    atomic.Bool
	closeOnce sync.Once
	errMu     sync.Mutex
	err       error
}

func newFFmpegDecoder(spec codecSpec, width, height, fps int, hw string) (*ffmpegDecoder, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, ErrFFmpegUnavailable
	}

	decoderName := strings.ToLower(strings.TrimPrefix(spec.mimeType, "video/"))
	if hw == "nvenc" {
		switch spec.mimeType {
		case webrtc.MimeTypeH264:
			decoderName = "h264_cuvid"
		case webrtc.MimeTypeVP8:
			decoderName = "vp8_cuvid"
		case webrtc.MimeTypeVP9:
			decoderName = "vp9_cuvid"
		}
	}

	outputPixFmt := "gray"
	frameSize := width * height

	args := []string{"-loglevel", "error", "-threads", "1"}
	if spec.mimeType == webrtc.MimeTypeH264 {
		args = append(args, "-f", "h264")
	} else {
		args = append(args, "-f", "ivf")
	}

	vfFilter := fmt.Sprintf("scale=%d:%d:flags=neighbor,format=%s", width, height, outputPixFmt)
	args = append(args,
		"-flags", "low_delay",
		"-vcodec", decoderName,
		"-i", "pipe:0",
		"-an",
		"-vf", vfFilter,
		"-pix_fmt", outputPixFmt,
		"-f", "rawvideo",
		"pipe:1",
	)

	cmd := exec.Command("ffmpeg", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("decoder stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("decoder stdout: %w", err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start decoder: %w", err)
	}

	dec := &ffmpegDecoder{
		cmd:       cmd,
		stdin:     stdin,
		stderr:    stderr,
		frames:    make(chan []byte, 32),
		mimeType:  spec.mimeType,
		frameSize: frameSize,
	}

	if spec.mimeType != webrtc.MimeTypeH264 {
		if err := writeIVFHeader(stdin, spec.fourCC, width, height, fps); err != nil {
			_ = dec.Close()
			return nil, fmt.Errorf("decoder ivf header: %w", err)
		}
	}

	go dec.readRawFrames(stdout)
	return dec, nil
}

func (d *ffmpegDecoder) PushSample(sample []byte) error {
	if err := d.processErr(); err != nil {
		return err
	}
	if d.mimeType == webrtc.MimeTypeH264 {
		if err := writeAll(d.stdin, sample); err != nil {
			return fmt.Errorf("write h264 decoder frame: %w", err)
		}
	} else {
		if err := writeIVFFrame(d.stdin, d.pts, sample); err != nil {
			return fmt.Errorf("write ivf decoder frame: %w", err)
		}
		d.pts++
	}
	return nil
}

func (d *ffmpegDecoder) PopFrame() ([]byte, error) {
	select {
	case frame, ok := <-d.frames:
		if !ok {
			return nil, d.processErr()
		}
		return frame, nil
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("pop frame timeout")
	}
}

func (d *ffmpegDecoder) Close() error {
	d.closeOnce.Do(func() {
		d.closed.Store(true)
		_ = d.stdin.Close()
		if d.cmd.Process != nil {
			_ = d.cmd.Process.Kill()
		}
		_ = d.cmd.Wait()
	})
	return nil
}

func (d *ffmpegDecoder) readRawFrames(stdout io.Reader) {
	defer close(d.frames)
	buf := make([]byte, d.frameSize)
	for {
		if _, err := io.ReadFull(stdout, buf); err != nil {
			if !d.closed.Load() {
				d.setErr(fmt.Errorf("decoder raw read: %w", err))
			}
			return
		}
		copyFrame := append([]byte(nil), buf...)
		if d.closed.Load() {
			return
		}
		d.frames <- copyFrame
	}
}

func (d *ffmpegDecoder) setErr(err error) {
	if err == nil {
		return
	}
	d.errMu.Lock()
	defer d.errMu.Unlock()
	if d.err == nil {
		d.err = withStderr(err, d.stderr)
	}
}

func (d *ffmpegDecoder) processErr() error {
	d.errMu.Lock()
	defer d.errMu.Unlock()
	if d.err != nil {
		return d.err
	}
	if d.closed.Load() {
		return ErrTransportClosed
	}
	return nil
}

func withStderr(err error, stderr *bytes.Buffer) error {
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, msg)
}

func writeIVFHeader(w io.Writer, fourCC string, width, height, frameRate int) error {
	header := make([]byte, 32)
	copy(header[0:4], []byte("DKIF"))
	binary.LittleEndian.PutUint16(header[4:6], 0)
	binary.LittleEndian.PutUint16(header[6:8], 32)
	copy(header[8:12], []byte(fourCC))
	binary.LittleEndian.PutUint16(header[12:14], uint16(width))
	binary.LittleEndian.PutUint16(header[14:16], uint16(height))
	binary.LittleEndian.PutUint32(header[16:20], uint32(frameRate))
	binary.LittleEndian.PutUint32(header[20:24], 1)
	binary.LittleEndian.PutUint32(header[24:28], 0)
	binary.LittleEndian.PutUint32(header[28:32], 0)
	return writeAll(w, header)
}

func writeIVFFrame(w io.Writer, pts uint64, frame []byte) error {
	header := make([]byte, 12)
	binary.LittleEndian.PutUint32(header[0:4], uint32(len(frame)))
	binary.LittleEndian.PutUint64(header[4:12], pts)
	if err := writeAll(w, header); err != nil {
		return err
	}
	return writeAll(w, frame)
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}
