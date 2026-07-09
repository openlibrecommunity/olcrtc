package multipath

import "testing"

func TestParsePathHello_RoundTrip(t *testing.T) {
	want := helloFrame{bondID: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, pathIndex: 3, numPaths: 4}
	raw := encodeHello(want)
	id, idx, num, ok := ParsePathHello(raw)
	if !ok {
		t.Fatal("ParsePathHello returned ok=false for a valid hello")
	}
	if id != want.bondID || idx != want.pathIndex || num != want.numPaths {
		t.Fatalf("ParsePathHello = (%x,%d,%d), want (%x,%d,%d)", id, idx, num, want.bondID, want.pathIndex, want.numPaths)
	}
}

func TestParsePathHello_Rejects(t *testing.T) {
	cases := map[string][]byte{
		"empty":      {},
		"wrong-type": append([]byte{byte(frameTypeData)}, make([]byte, helloFrameSize-1)...),
		"too-short":  make([]byte, helloFrameSize-1),
		"too-long":   make([]byte, helloFrameSize+1),
		"data-frame": encodeData(7, []byte("payload-bytes-not-a-hello")),
		"ack-frame":  encodeAck(9),
	}
	for name, raw := range cases {
		if _, _, _, ok := ParsePathHello(raw); ok {
			t.Errorf("%s: ParsePathHello returned ok=true, want false", name)
		}
	}
}
