package transport

import (
	"fmt"
	"image"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/visual/codec"
)

// Sender is the encode-side bridge from application messages to visual codec
// frames. It owns fragmentation/FEC framing and then maps each wire slot into
// one codec frame.
type Sender struct {
	fragmenter *Fragmenter
}

// NewSender creates a visual transport sender using the given parity ratio
// percent (40 = 40% redundancy).
func NewSender(parityRatioPercent int) *Sender {
	return &Sender{
		fragmenter: NewFragmenter(parityRatioPercent),
	}
}

// EncodeFrames turns one application payload into a sequence of codec-ready
// frames. Each returned frame carries exactly one slot.
func (s *Sender) EncodeFrames(payload []byte) ([]*image.YCbCr, error) {
	slots, err := s.fragmenter.Fragment(payload)
	if err != nil {
		return nil, fmt.Errorf("fragment payload into slots: %w", err)
	}

	frames := make([]*image.YCbCr, len(slots))
	for i, slot := range slots {
		frame, err := codec.Encode(slot)
		if err != nil {
			return nil, fmt.Errorf("encode slot %d as codec frame: %w", i, err)
		}
		frames[i] = frame
	}
	return frames, nil
}

// Receiver is the decode-side bridge from visual codec frames back to
// application messages. It owns codec decode and slot reassembly.
type Receiver struct {
	reassembler *Reassembler
}

// NewReceiver creates a receiver with the default partial-message timeout.
func NewReceiver() *Receiver {
	return &Receiver{
		reassembler: NewReassembler(),
	}
}

// NewReceiverWithTimeout creates a receiver with a custom timeout for stale
// partial messages.
func NewReceiverWithTimeout(timeout time.Duration) *Receiver {
	return &Receiver{
		reassembler: NewReassemblerWithTimeout(timeout),
	}
}

// AcceptFrame decodes one codec frame, feeds the resulting slot into the
// reassembler, and returns a completed application payload if this frame closes
// the message.
func (r *Receiver) AcceptFrame(frame *image.YCbCr) ([]byte, bool, error) {
	slot, err := codec.Decode(frame)
	if err != nil {
		return nil, false, fmt.Errorf("decode codec frame: %w", err)
	}
	return r.reassembler.Accept(slot)
}

// SweepExpired drops stale partial messages and returns the number removed.
func (r *Receiver) SweepExpired() int {
	return r.reassembler.SweepExpired()
}
