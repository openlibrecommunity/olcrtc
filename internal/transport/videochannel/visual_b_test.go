//go:build b

package videochannel

import (
	"bytes"
	"testing"
)

func TestBCodecRoundtrip(t *testing.T) {
	data := []byte("Hello JABCode test 123456789012345678901234567890")
	width, height := 480, 480

	frame, err := renderVisualFrameB(data, width, height)
	if err != nil {
		t.Fatalf("renderVisualFrameB failed: %v", err)
	}
	expectedSize := width * height * 4
	if len(frame) != expectedSize {
		t.Fatalf("unexpected frame size: %d, want %d", len(frame), expectedSize)
	}

	payload, err := extractVisualPayloadB(frame, width, height)
	if err != nil {
		t.Fatalf("extractVisualPayloadB failed: %v", err)
	}
	if payload == nil {
		t.Fatal("extractVisualPayloadB returned nil payload")
	}

	if !bytes.Equal(payload, data) {
		t.Fatalf("roundtrip mismatch:\noriginal: %q\ndecoded:  %q", string(data), string(payload))
	}
}

func TestBCodecEmptyPayload(t *testing.T) {
	width, height := 480, 480

	frame, err := renderVisualFrameB(nil, width, height)
	if err != nil {
		t.Fatalf("renderVisualFrameB with empty payload failed: %v", err)
	}
	expectedSize := width * height * 4
	if len(frame) != expectedSize {
		t.Fatalf("unexpected frame size: %d", len(frame))
	}
}
