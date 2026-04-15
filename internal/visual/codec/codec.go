// Package codec converts application bytes to and from YCbCr video frames
// using a block-based luma modulation scheme.
//
// # Why a custom codec
//
// The visual transport wants to smuggle bytes through a conventional WebRTC
// video track. Modern SFUs reserve the right to re-encode or transrate video,
// which destroys most kinds of per-pixel steganography. What they *do*
// preserve reasonably well is the gross luma structure of each macroblock,
// because that is what the downstream codec (VP8/H.264) is designed to
// reproduce. So we encode bits as the average luma of 16×16 blocks — exactly
// the shape of a VP8 macroblock, which means the codec will not "spread"
// neighbouring cells into each other on re-encode.
//
// # Scheme
//
// Frame is 640×480 in I420 (YCbCr 4:2:0). We use 40×30 = 1200 blocks of
// 16×16 pixels. Each block encodes one symbol from a 4-level luma palette,
// so each block carries 2 bits. The chroma planes are flat-filled with the
// neutral value 128 (grey) — 4:2:0 subsampling makes chroma too lossy for
// data, and a neutral grey gives the SFU's encoder something trivial to
// compress.
//
// Block reservation (out of 1200):
//
//	4 corner sync markers — geometry lock on decode
//	8 blocks for a uint16 length prefix (payload byte length)
//	1188 blocks for payload = 297 bytes/frame of raw capacity
//
// At 15 fps that is ~35 kbps of raw bandwidth before FEC. Once we add inner
// Reed-Solomon (10+4, ~40% overhead) and outer interleave, the delivered
// application bandwidth lands in the 15–25 kbps range — consistent with the
// plan's PoC target.
//
// # Luma palette
//
// We split the legal VP8/H.264 luma range [16, 240] into four equal buckets
// and encode a symbol as the *centre* of its bucket. Decode takes the mean
// luma of the block centre and snaps to the nearest bucket centre.
//
//	symbol 0b00 → Y = 44   (bucket [16,  72))
//	symbol 0b01 → Y = 100  (bucket [72,  128))
//	symbol 0b10 → Y = 156  (bucket [128, 184))
//	symbol 0b11 → Y = 212  (bucket [184, 240])
//
// Using centres instead of bucket edges maximises the Hamming distance in
// luma space (56 units between neighbouring symbols) so moderate re-quantise
// noise can still be recovered.
//
// # Sync corners
//
// Four corner blocks carry a fixed pattern that does not depend on the
// payload: TL = 16, TR = 240, BL = 240, BR = 16 (i.e. dark / light / light /
// dark). A decoder that has lost geometry lock can scan a small window
// looking for that 2×2 symbol pattern and re-align. For the offline test
// harness of this package we do not actually search — dimensions are known —
// but we *do* write and verify the pattern so the decoder can catch
// mis-aligned inputs and refuse to decode garbage.
package codec

import (
	"errors"
	"image"
)

// Frame geometry. These are compile-time constants because the whole codec
// is sized off them and tests lean on the exact numbers.
const (
	// FrameWidth is the fixed frame width in pixels.
	FrameWidth = 640
	// FrameHeight is the fixed frame height in pixels.
	FrameHeight = 480

	// BlockSize is the side length of one data block in pixels. Must match
	// the VP8 macroblock size (16) or the downstream encoder may smear
	// neighbouring blocks into each other during intra prediction.
	BlockSize = 16

	// BlocksX / BlocksY / BlocksTotal describe the 2D block grid.
	BlocksX     = FrameWidth / BlockSize  // 40
	BlocksY     = FrameHeight / BlockSize // 30
	BlocksTotal = BlocksX * BlocksY       // 1200

	// Reserved block counts (see package doc).
	SyncBlocks   = 4
	LengthBlocks = 8 // 16 bits / 2 bits per block

	// DataBlocks is the number of blocks available for payload symbols.
	DataBlocks = BlocksTotal - SyncBlocks - LengthBlocks // 1188

	// BitsPerBlock is the luma-palette symbol width.
	BitsPerBlock = 2

	// MaxPayloadBytes is the theoretical per-frame capacity before any
	// FEC overhead. 1188 blocks × 2 bits / 8 = 297 bytes.
	MaxPayloadBytes = DataBlocks * BitsPerBlock / 8 // 297
)

const (
	paletteY0 uint8 = 44
	paletteY1 uint8 = 100
	paletteY2 uint8 = 156
	paletteY3 uint8 = 212

	bucketEdge0 uint8 = 72
	bucketEdge1 uint8 = 128
	bucketEdge2 uint8 = 184

	syncCornerTL uint8 = 0b00
	syncCornerTR uint8 = 0b11
	syncCornerBL uint8 = 0b11
	syncCornerBR uint8 = 0b00
)

// ErrLengthMismatch is returned when decoded length prefix does not fit.
var ErrLengthMismatch = errors.New("codec: decoded length exceeds payload capacity")

// ErrSyncMismatch is returned when the sync corners do not match the
// expected pattern. For the offline codec this means the frame buffer was
// fed to Decode without being produced by Encode — in production it would
// also mean we have lost geometry lock after an SFU re-encode.
var ErrSyncMismatch = errors.New("codec: sync corner pattern mismatch")

// ErrPayloadTooLarge is returned from Encode when the input exceeds the
// per-frame capacity. Callers should fragment upstream.
var ErrPayloadTooLarge = errors.New("codec: payload exceeds MaxPayloadBytes")

// Encode draws the payload as a 640×480 I420 frame. The returned YCbCr
// image owns its pixel buffers; callers may mutate them freely (e.g. to
// blend a background frame on top of the raw sketch).
//
// The input is not fragmented — that responsibility lives one layer up in
// the visual transport (media_engine_visual). Encode rejects anything
// larger than MaxPayloadBytes.
func Encode(payload []byte) (*image.YCbCr, error) {
	if len(payload) > MaxPayloadBytes {
		return nil, ErrPayloadTooLarge
	}

	frame := newBlankFrame()

	// Linear block index after we skip sync corners. block(0)..block(3)
	// are the corners, so data starts at block(4).
	writer := newBlockWriter(frame)

	// 1. Write sync corners in fixed positions.
	writer.writeSyncCorners()

	// 2. Write the 16-bit length prefix as the first LengthBlocks blocks
	// *after* the sync corners. We serialise MSB-first so the decoder
	// can read it as big-endian without ambiguity.
	length := uint16(len(payload)) //nolint:gosec // len is bounded by MaxPayloadBytes (297)
	writer.writeUint16(length)

	// 3. Write the payload bits. Any remaining blocks are left at their
	// neutral luma value (paletteY0) — this keeps the frame uniform
	// for the downstream encoder and does not leak bits about payload
	// length beyond what the length prefix already revealed.
	writer.writePayload(payload)

	return frame, nil
}

// Decode extracts the payload from a frame previously produced by Encode,
// tolerating light per-block luma drift. It returns ErrSyncMismatch if the
// corner pattern is not recognisable.
//
// Implementation note: we read the *mean* luma of a centre sub-block of each
// 16×16 tile, not the four corner pixels. The centre is more robust against
// any sort of block-edge ringing a downstream codec might introduce.
func Decode(frame *image.YCbCr) ([]byte, error) {
	reader := newBlockReader(frame)

	// 1. Validate sync corners. We are strict here in offline tests —
	// any drift is a bug. The production decoder will instead search a
	// small window for the pattern.
	if !reader.checkSyncCorners() {
		return nil, ErrSyncMismatch
	}

	// 2. Read length prefix.
	length := reader.readUint16()
	if int(length) > MaxPayloadBytes {
		return nil, ErrLengthMismatch
	}

	// 3. Read exactly `length` payload bytes, ignoring any trailing
	// blocks (they are padding).
	return reader.readPayload(int(length)), nil
}

// newBlankFrame allocates an I420 frame with Y=paletteY[0] and flat grey
// chroma. Using the lowest luma value as background means any "missing"
// block on the receiving side decodes to symbol 0 instead of garbage — that
// is easier to spot as padding when debugging the raw bit stream.
func newBlankFrame() *image.YCbCr {
	rect := image.Rect(0, 0, FrameWidth, FrameHeight)
	frame := image.NewYCbCr(rect, image.YCbCrSubsampleRatio420)

	// Y plane: init to palette 0.
	for i := range frame.Y {
		frame.Y[i] = paletteY0
	}
	// Chroma planes: flat neutral (128). Not used for data.
	for i := range frame.Cb {
		frame.Cb[i] = 128
	}
	for i := range frame.Cr {
		frame.Cr[i] = 128
	}

	return frame
}

// blockWriter walks the block grid in row-major order and writes symbols.
// It hides the sync-corner reservation so higher-level code can treat the
// grid as a flat sequence of data blocks.
type blockWriter struct {
	frame *image.YCbCr
	// cursor is the current data-block index (0..DataBlocks+LengthBlocks).
	cursor int
}

func newBlockWriter(frame *image.YCbCr) *blockWriter {
	return &blockWriter{frame: frame}
}

// writeSyncCorners stamps the fixed pattern into the four grid corners.
// Those slots are skipped by dataBlockCoords during payload write.
func (w *blockWriter) writeSyncCorners() {
	for i, sym := range syncCornerSymbols() {
		bx, by := syncCornerCoords(i)
		writeBlockSymbol(w.frame, bx, by, sym)
	}
}

// writeUint16 writes a 16-bit value MSB-first as 8 consecutive 2-bit symbols.
func (w *blockWriter) writeUint16(v uint16) {
	for i := range LengthBlocks {
		// Extract symbol `i` (MSB-first): take the top two bits, shift.
		shift := symbolShift(i)
		symbol := uint8((v >> shift) & 0b11)

		bx, by := w.nextCoord()
		writeBlockSymbol(w.frame, bx, by, symbol)
	}
}

// writePayload serialises payload bytes as 2-bit symbols, MSB first within
// each byte. Trailing blocks stay at their init value.
func (w *blockWriter) writePayload(payload []byte) {
	for _, b := range payload {
		// 4 symbols per byte, high bits first.
		for shift := 6; shift >= 0; shift -= 2 {
			symbol := (b >> uint(shift)) & 0b11
			bx, by := w.nextCoord()
			writeBlockSymbol(w.frame, bx, by, symbol)
		}
	}
}

// nextCoord returns the next (bx, by) grid position skipping the sync
// corners. Flat data-block indices are dense, so the caller never needs to
// think about the skip.
func (w *blockWriter) nextCoord() (int, int) {
	bx, by := dataBlockCoords(w.cursor)
	w.cursor++
	return bx, by
}

// blockReader is the decode-side counterpart of blockWriter. Symmetric API.
type blockReader struct {
	frame  *image.YCbCr
	cursor int
}

func newBlockReader(frame *image.YCbCr) *blockReader {
	return &blockReader{frame: frame}
}

func (r *blockReader) checkSyncCorners() bool {
	for i, expect := range syncCornerSymbols() {
		bx, by := syncCornerCoords(i)
		got := readBlockSymbol(r.frame, bx, by)
		if got != expect {
			return false
		}
	}
	return true
}

func (r *blockReader) readUint16() uint16 {
	var v uint16
	for i := range LengthBlocks {
		bx, by := r.nextCoord()
		sym := readBlockSymbol(r.frame, bx, by)
		shift := symbolShift(i)
		v |= uint16(sym) << shift
	}
	return v
}

func (r *blockReader) readPayload(length int) []byte {
	out := make([]byte, length)
	for i := range length {
		var b uint8
		for shift := 6; shift >= 0; shift -= 2 {
			bx, by := r.nextCoord()
			sym := readBlockSymbol(r.frame, bx, by)
			b |= sym << uint(shift)
		}
		out[i] = b
	}
	return out
}

func (r *blockReader) nextCoord() (int, int) {
	bx, by := dataBlockCoords(r.cursor)
	r.cursor++
	return bx, by
}

// syncCornerCoords returns the (bx, by) of the i-th sync corner.
// Order matches syncCorners: TL, TR, BL, BR.
func syncCornerCoords(i int) (int, int) {
	switch i {
	case 0:
		return 0, 0
	case 1:
		return BlocksX - 1, 0
	case 2:
		return 0, BlocksY - 1
	default:
		return BlocksX - 1, BlocksY - 1
	}
}

// dataBlockCoords maps a flat data-block index (0-based, skipping the four
// sync corners) to a (bx, by) grid position. It walks row-major and skips
// any corner that lies on the current step.
func dataBlockCoords(flat int) (int, int) {
	// Walk the grid and count non-corner slots. This is O(BlocksTotal) in
	// the worst case but the total is only 1200 and we never call it
	// hot-loop at runtime during transmission — encode/decode run in
	// dedicated goroutines behind the send queue.
	//
	// A cleverer implementation could compute the position directly with
	// a handful of branches (only 4 corners, always at (0,0), (0,Y-1),
	// (X-1,0), (X-1,Y-1)), but correctness first.
	seen := 0
	for by := range BlocksY {
		for bx := range BlocksX {
			if isSyncCorner(bx, by) {
				continue
			}
			if seen == flat {
				return bx, by
			}
			seen++
		}
	}
	// Overflow: should never happen inside the declared capacity.
	return -1, -1
}

func isSyncCorner(bx, by int) bool {
	return (bx == 0 && by == 0) ||
		(bx == BlocksX-1 && by == 0) ||
		(bx == 0 && by == BlocksY-1) ||
		(bx == BlocksX-1 && by == BlocksY-1)
}

// writeBlockSymbol fills the entire 16×16 region at grid (bx, by) with the
// luma value corresponding to the given symbol.
func writeBlockSymbol(frame *image.YCbCr, bx, by int, symbol uint8) {
	y := paletteValue(symbol)
	px := bx * BlockSize
	py := by * BlockSize

	for row := range BlockSize {
		// YStride is the distance (in bytes) between the start of one
		// row of the Y plane and the next. On a 640-wide frame it
		// equals 640 but we do not hard-code that in case tests want
		// to pad.
		offset := (py+row)*frame.YStride + px
		rowSlice := frame.Y[offset : offset+BlockSize]
		for i := range rowSlice {
			rowSlice[i] = y
		}
	}
}

// readBlockSymbol returns the decoded symbol at grid (bx, by). It averages
// the centre 8×8 sub-block, which is less affected by codec edge artefacts
// than the full 16×16 region, and then snaps to the nearest palette bucket.
func readBlockSymbol(frame *image.YCbCr, bx, by int) uint8 {
	const centreSize = 8
	const offsetPx = (BlockSize - centreSize) / 2 // 4

	px := bx*BlockSize + offsetPx
	py := by*BlockSize + offsetPx

	var sum uint32
	for row := range centreSize {
		offset := (py+row)*frame.YStride + px
		rowSlice := frame.Y[offset : offset+centreSize]
		for _, v := range rowSlice {
			sum += uint32(v)
		}
	}
	avg := uint8(sum / (centreSize * centreSize))
	return snap(avg)
}

// snap maps an arbitrary luma value to the nearest palette symbol. Bucket
// edges are pre-computed so this is just three compares.
func snap(y uint8) uint8 {
	switch {
	case y < bucketEdge0:
		return 0b00
	case y < bucketEdge1:
		return 0b01
	case y < bucketEdge2:
		return 0b10
	default:
		return 0b11
	}
}

func syncCornerSymbols() [4]uint8 {
	return [4]uint8{syncCornerTL, syncCornerTR, syncCornerBL, syncCornerBR}
}

func symbolShift(i int) uint {
	switch i {
	case 0:
		return 14
	case 1:
		return 12
	case 2:
		return 10
	case 3:
		return 8
	case 4:
		return 6
	case 5:
		return 4
	case 6:
		return 2
	default:
		return 0
	}
}

func paletteValue(symbol uint8) uint8 {
	switch symbol & 0b11 {
	case 0b00:
		return paletteY0
	case 0b01:
		return paletteY1
	case 0b10:
		return paletteY2
	default:
		return paletteY3
	}
}
