package jazz

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestVisualEngineSetupInstallsVideoTrack(t *testing.T) {
	pcPub, pcSub := mustCreateVisualTestPCs(t)
	engine, readyCh := mustSetupVisualEngine(t, pcPub, pcSub)
	assertVisualEngineNotReadyYet(t, readyCh)
	assertVisualEngineLocalTrack(t, engine)
	assertVisualEngineNoSendersBeforeAttach(t, pcPub)
	attachAudioTrackAndAssert(t, engine, pcPub)
	attachVideoTrackAndAssert(t, engine, pcPub)
	assertVisualEngineReadyAfterMark(t, engine, readyCh)
}

func mustCreateVisualTestPCs(t *testing.T) (*webrtc.PeerConnection, *webrtc.PeerConnection) {
	t.Helper()
	api := webrtc.NewAPI()
	pcPub, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("create publisher pc: %v", err)
	}
	t.Cleanup(func() { _ = pcPub.Close() })
	pcSub, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("create subscriber pc: %v", err)
	}
	t.Cleanup(func() { _ = pcSub.Close() })
	return pcPub, pcSub
}

func mustSetupVisualEngine(t *testing.T, pcPub, pcSub *webrtc.PeerConnection) (*visualEngine, <-chan struct{}) {
	t.Helper()
	engine := newVisualEngine()
	readyCh, err := engine.setup(pcPub, pcSub)
	if err != nil {
		t.Fatalf("setup visual engine: %v", err)
	}
	if readyCh == nil {
		t.Fatal("setup returned nil ready channel")
	}
	return engine, readyCh
}

func assertVisualEngineNotReadyYet(t *testing.T, readyCh <-chan struct{}) {
	t.Helper()
	select {
	case <-readyCh:
		t.Fatal("visual engine ready channel closed before publisher answer")
	case <-time.After(100 * time.Millisecond):
	}
}

func assertVisualEngineLocalTrack(t *testing.T, engine *visualEngine) {
	t.Helper()
	if engine.localTrack == nil {
		t.Fatal("visual engine did not keep local track")
	}
	if got := engine.localTrack.Codec().MimeType; got != webrtc.MimeTypeVP8 {
		t.Fatalf("local track codec = %q, want %q", got, webrtc.MimeTypeVP8)
	}
}

func assertVisualEngineNoSendersBeforeAttach(t *testing.T, pcPub *webrtc.PeerConnection) {
	t.Helper()
	if got := len(pcPub.GetSenders()); got != 0 {
		t.Fatalf("publisher pc senders after setup = %d, want 0 before attach", got)
	}
}

func attachAudioTrackAndAssert(t *testing.T, engine *visualEngine, pcPub *webrtc.PeerConnection) {
	t.Helper()
	audioMeta := engine.nextTrackMetadata()
	if got, want := audioMeta["type"], "AUDIO"; got != want {
		t.Fatalf("first track type = %v, want %s", got, want)
	}
	engine.onTrackPublished(engine.audioTrackCID, "audio-sid")
	if err := engine.attachPublisherTrack(pcPub); err != nil {
		t.Fatalf("attach publisher audio track: %v", err)
	}
	if got := len(pcPub.GetSenders()); got != 1 {
		t.Fatalf("publisher pc senders after audio attach = %d, want 1", got)
	}
}

func attachVideoTrackAndAssert(t *testing.T, engine *visualEngine, pcPub *webrtc.PeerConnection) {
	t.Helper()
	videoMeta := engine.nextTrackMetadata()
	if got, want := videoMeta["type"], "VIDEO"; got != want {
		t.Fatalf("second track type = %v, want %s", got, want)
	}
	engine.onTrackPublished(engine.trackCID, "video-sid")
	if err := engine.attachPublisherTrack(pcPub); err != nil {
		t.Fatalf("attach publisher video track: %v", err)
	}
	if got := len(pcPub.GetSenders()); got != 2 {
		t.Fatalf("publisher pc senders after video attach = %d, want 2", got)
	}
}

func assertVisualEngineReadyAfterMark(t *testing.T, engine *visualEngine, readyCh <-chan struct{}) {
	t.Helper()
	engine.markPublisherReady()
	select {
	case <-readyCh:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("visual engine ready channel did not close after publisher ready")
	}
}
