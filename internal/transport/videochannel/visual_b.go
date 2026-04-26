//go:build b

package videochannel

import (
	"fmt"

	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/zarazaex69/b/go"
)

func renderVisualFrameB(payload []byte, width, height int) ([]byte, error) {
	rgba := make([]byte, width*height*4)
	for i := 0; i < len(rgba); i += 4 {
		rgba[i] = 0xff
		rgba[i+1] = 0xff
		rgba[i+2] = 0xff
		rgba[i+3] = 0xff
	}

	if len(payload) == 0 {
		return rgba, nil
	}

	cfg := b.DefaultConfig()
	result, err := b.Encode(payload, cfg)
	if err != nil {
		return nil, fmt.Errorf("b encode: %w", err)
	}

	bmpW := int(result.Width)
	bmpH := int(result.Height)
	offsetX := (width - bmpW) / 2
	offsetY := (height - bmpH) / 2

	for y := 0; y < bmpH; y++ {
		for x := 0; x < bmpW; x++ {
			srcIdx := (y*bmpW + x) * 4
			pixelX := offsetX + x
			pixelY := offsetY + y
			if pixelX >= 0 && pixelX < width && pixelY >= 0 && pixelY < height {
				dstIdx := (pixelY*width + pixelX) * 4
				rgba[dstIdx] = result.RGBA[srcIdx]
				rgba[dstIdx+1] = result.RGBA[srcIdx+1]
				rgba[dstIdx+2] = result.RGBA[srcIdx+2]
				rgba[dstIdx+3] = result.RGBA[srcIdx+3]
			}
		}
	}

	return rgba, nil
}

func extractVisualPayloadB(frame []byte, width, height int) ([]byte, error) {
	expectedSize := width * height * 4
	if len(frame) != expectedSize {
		return nil, fmt.Errorf("unexpected frame size: %d (expected %dx%dx4=%d)", len(frame), width, height, expectedSize)
	}

	cfg := b.DefaultConfig()
	decoded, err := b.Decode(frame, uint32(width), uint32(height), cfg)
	if err != nil {
		logger.Debugf("b decode failed: %v", err)
		return nil, nil
	}

	return decoded, nil
}
