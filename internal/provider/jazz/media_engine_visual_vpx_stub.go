//go:build !vpx

package jazz

import "github.com/pion/webrtc/v4"

// The non-vpx build keeps the legacy frame/sample harness active only so the
// visual transport stays testable on machines without libvpx. This file is a
// fallback shim, not the intended runtime path.

func newVisualVP8SampleCodec() (visualSampleCodec, error) {
	return nil, errVisualVP8Unavailable
}

func newVisualVP8FrameReader(_ *webrtc.TrackRemote) (visualFrameReader, error) {
	return nil, errVisualVP8Unavailable
}
