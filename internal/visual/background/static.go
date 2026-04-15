// Package background loads static I420 carrier frames for visual disguise mode.
package background

import (
	"errors"
	"fmt"
	"image"
	"os"

	visualcodec "github.com/openlibrecommunity/olcrtc/internal/visual/codec"
)

const i420FrameSize = visualcodec.FrameWidth*visualcodec.FrameHeight +
	2*(visualcodec.FrameWidth/2)*(visualcodec.FrameHeight/2)

var errBackgroundFileTooShort = errors.New("background file too short")

// LoadStaticI420 loads a single 640x480 I420 frame from a raw .yuv file.
func LoadStaticI420(path string) (*image.YCbCr, error) {
	// #nosec G304 -- background path is explicitly configured by the local operator.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read background file: %w", err)
	}
	if len(data) < i420FrameSize {
		return nil, fmt.Errorf(
			"%w: got %d need %d",
			errBackgroundFileTooShort,
			len(data),
			i420FrameSize,
		)
	}

	rect := image.Rect(0, 0, visualcodec.FrameWidth, visualcodec.FrameHeight)
	frame := image.NewYCbCr(rect, image.YCbCrSubsampleRatio420)

	ySize := visualcodec.FrameWidth * visualcodec.FrameHeight
	cSize := (visualcodec.FrameWidth / 2) * (visualcodec.FrameHeight / 2)

	copy(frame.Y, data[:ySize])
	copy(frame.Cb, data[ySize:ySize+cSize])
	copy(frame.Cr, data[ySize+cSize:ySize+2*cSize])

	compressLumaHeadroom(frame.Y)
	return frame, nil
}

func compressLumaHeadroom(y []byte) {
	for i, v := range y {
		shifted := int(v) - 128
		y[i] = clampByte(128 + shifted/3)
	}
}

func clampByte(v int) uint8 {
	switch {
	case v < 16:
		return 16
	case v > 240:
		return 240
	default:
		return uint8(v)
	}
}
