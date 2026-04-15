// Package lav applies baseline-relative luma modulation for visual disguise mode.
package lav

import (
	"image"

	visualcodec "github.com/openlibrecommunity/olcrtc/internal/visual/codec"
)

const (
	centreSize  = 8
	centreShift = (visualcodec.BlockSize - centreSize) / 2
)

// Modulate applies carrier-relative luma deltas to a canonical payload frame.
func Modulate(carrier, payload *image.YCbCr) *image.YCbCr {
	if carrier == nil || payload == nil {
		return payload
	}
	if !supportsFrame(carrier) || !supportsFrame(payload) {
		return payload
	}

	dst := cloneYCbCr(carrier)
	for by := range visualcodec.BlocksY {
		for bx := range visualcodec.BlocksX {
			symbol := readSymbol(payload, bx, by)
			applyDelta(dst, carrier, bx, by, lavSymbolDelta(symbol))
		}
	}
	return dst
}

// Demodulate restores a canonical payload frame from carrier-modulated input.
func Demodulate(carrier, modulated *image.YCbCr) *image.YCbCr {
	if carrier == nil || modulated == nil {
		return modulated
	}
	if !supportsFrame(carrier) || !supportsFrame(modulated) {
		return modulated
	}

	rect := image.Rect(0, 0, visualcodec.FrameWidth, visualcodec.FrameHeight)
	dst := image.NewYCbCr(rect, image.YCbCrSubsampleRatio420)
	for i := range dst.Cb {
		dst.Cb[i] = 128
	}
	for i := range dst.Cr {
		dst.Cr[i] = 128
	}

	for by := range visualcodec.BlocksY {
		for bx := range visualcodec.BlocksX {
			symbol := readLavSymbol(carrier, modulated, bx, by)
			writeCanonicalBlock(dst, bx, by, symbol)
		}
	}
	return dst
}

func cloneYCbCr(src *image.YCbCr) *image.YCbCr {
	dst := image.NewYCbCr(src.Rect, src.SubsampleRatio)
	copy(dst.Y, src.Y)
	copy(dst.Cb, src.Cb)
	copy(dst.Cr, src.Cr)
	return dst
}

func applyDelta(dst, carrier *image.YCbCr, bx, by, delta int) {
	px := bx * visualcodec.BlockSize
	py := by * visualcodec.BlockSize
	for row := range visualcodec.BlockSize {
		offset := (py+row)*dst.YStride + px
		for col := range visualcodec.BlockSize {
			base := int(carrier.Y[offset+col])
			dst.Y[offset+col] = clampByte(base + delta)
		}
	}
}

func readSymbol(frame *image.YCbCr, bx, by int) int {
	avg := centreAverage(frame, bx, by)
	switch {
	case avg < 72:
		return 0
	case avg < 128:
		return 1
	case avg < 184:
		return 2
	default:
		return 3
	}
}

func readLavSymbol(carrier, modulated *image.YCbCr, bx, by int) int {
	base := centreAverage(carrier, bx, by)
	cur := centreAverage(modulated, bx, by)
	diff := cur - base
	switch {
	case diff < -48:
		return 0
	case diff < 0:
		return 1
	case diff < 48:
		return 2
	default:
		return 3
	}
}

func centreAverage(frame *image.YCbCr, bx, by int) int {
	px := bx*visualcodec.BlockSize + centreShift
	py := by*visualcodec.BlockSize + centreShift
	sum := 0
	for row := range centreSize {
		offset := (py+row)*frame.YStride + px
		for col := range centreSize {
			sum += int(frame.Y[offset+col])
		}
	}
	return sum / (centreSize * centreSize)
}

func supportsFrame(frame *image.YCbCr) bool {
	if frame == nil {
		return false
	}
	if frame.Rect.Dx() < visualcodec.FrameWidth || frame.Rect.Dy() < visualcodec.FrameHeight {
		return false
	}
	if frame.YStride < visualcodec.FrameWidth {
		return false
	}
	if len(frame.Y) < frame.YStride*visualcodec.FrameHeight {
		return false
	}
	return true
}

func writeCanonicalBlock(frame *image.YCbCr, bx, by, symbol int) {
	var y uint8
	switch symbol {
	case 0:
		y = 44
	case 1:
		y = 100
	case 2:
		y = 156
	default:
		y = 212
	}

	px := bx * visualcodec.BlockSize
	py := by * visualcodec.BlockSize
	for row := range visualcodec.BlockSize {
		offset := (py+row)*frame.YStride + px
		for col := range visualcodec.BlockSize {
			frame.Y[offset+col] = y
		}
	}
}

func lavSymbolDelta(symbol int) int {
	switch symbol {
	case 0:
		return -72
	case 1:
		return -24
	case 2:
		return 24
	default:
		return 72
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
