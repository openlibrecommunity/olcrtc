package multipath

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// frameType identifies the kind of Bond-protocol frame carried inside a
// single path Send/OnData message. Bond frames ride on top of an already
// reliable, ordered per-path transport (KCP/SCTP) - Bond does not implement
// its own retransmission-on-loss for a live path, only rerouting when a
// whole path dies.
type frameType byte

const (
	// frameTypeHello announces a path joining a bond. Sent once per path at
	// Connect/AddPath time so the peer (in particular a future server-side
	// bond registry) can group paths by bond ID.
	frameTypeHello frameType = 1
	// frameTypeData carries one striped chunk of the aggregate byte stream.
	frameTypeData frameType = 2
	// frameTypeAck carries a cumulative sequence acknowledgement so the
	// sender can drop the resend buffer entries it no longer needs.
	frameTypeAck frameType = 3
)

const (
	// frameTypeSize is the leading type byte common to every frame.
	frameTypeSize = 1

	// helloFrameSize is the fixed wire size of a PATH_HELLO frame:
	// type(1) + bond_id(16) + path_index(2) + num_paths(2).
	helloFrameSize = frameTypeSize + 16 + 2 + 2

	// dataFrameHeaderSize is the fixed header size of a DATA frame before
	// the variable-length payload: type(1) + seq(8).
	dataFrameHeaderSize = frameTypeSize + 8

	// ackFrameSize is the fixed wire size of an ACK frame: type(1) + cum_seq(8).
	ackFrameSize = frameTypeSize + 8
)

// ErrFrameTooShort is returned when a received frame is smaller than its
// type demands.
var ErrFrameTooShort = errors.New("multipath: frame too short")

// ErrUnknownFrameType is returned when a frame's leading byte does not match
// any known frameType.
var ErrUnknownFrameType = errors.New("multipath: unknown frame type")

// helloFrame is the decoded form of a PATH_HELLO frame.
type helloFrame struct {
	bondID    [16]byte
	pathIndex uint16
	numPaths  uint16
}

// peekFrameType reports the frame type of a raw wire frame without fully
// decoding it.
func peekFrameType(raw []byte) (frameType, error) {
	if len(raw) < frameTypeSize {
		return 0, ErrFrameTooShort
	}
	return frameType(raw[0]), nil
}

// encodeHello serialises a PATH_HELLO frame.
func encodeHello(f helloFrame) []byte {
	buf := make([]byte, helloFrameSize)
	buf[0] = byte(frameTypeHello)
	copy(buf[1:17], f.bondID[:])
	binary.BigEndian.PutUint16(buf[17:19], f.pathIndex)
	binary.BigEndian.PutUint16(buf[19:21], f.numPaths)
	return buf
}

// decodeHello parses a PATH_HELLO frame. raw must include the leading type byte.
func decodeHello(raw []byte) (helloFrame, error) {
	if len(raw) < helloFrameSize {
		return helloFrame{}, fmt.Errorf("%w: hello needs %d bytes, got %d", ErrFrameTooShort, helloFrameSize, len(raw))
	}
	var f helloFrame
	copy(f.bondID[:], raw[1:17])
	f.pathIndex = binary.BigEndian.Uint16(raw[17:19])
	f.numPaths = binary.BigEndian.Uint16(raw[19:21])
	return f, nil
}

// encodeData serialises a DATA frame around payload. The returned slice
// owns a fresh backing array; payload is copied into it.
func encodeData(seq uint64, payload []byte) []byte {
	buf := make([]byte, dataFrameHeaderSize+len(payload))
	buf[0] = byte(frameTypeData)
	binary.BigEndian.PutUint64(buf[1:9], seq)
	copy(buf[9:], payload)
	return buf
}

// decodeData parses a DATA frame. The returned payload aliases raw - callers
// that retain it past the lifetime of raw must copy it first.
func decodeData(raw []byte) (seq uint64, payload []byte, err error) {
	if len(raw) < dataFrameHeaderSize {
		return 0, nil, fmt.Errorf("%w: data needs %d bytes, got %d", ErrFrameTooShort, dataFrameHeaderSize, len(raw))
	}
	seq = binary.BigEndian.Uint64(raw[1:9])
	payload = raw[9:]
	return seq, payload, nil
}

// encodeAck serialises an ACK frame carrying the cumulative sequence.
func encodeAck(cumSeq uint64) []byte {
	buf := make([]byte, ackFrameSize)
	buf[0] = byte(frameTypeAck)
	binary.BigEndian.PutUint64(buf[1:9], cumSeq)
	return buf
}

// decodeAck parses an ACK frame.
func decodeAck(raw []byte) (cumSeq uint64, err error) {
	if len(raw) < ackFrameSize {
		return 0, fmt.Errorf("%w: ack needs %d bytes, got %d", ErrFrameTooShort, ackFrameSize, len(raw))
	}
	return binary.BigEndian.Uint64(raw[1:9]), nil
}
