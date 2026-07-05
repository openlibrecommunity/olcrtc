package livekit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/openlibrecommunity/olcrtc/internal/engine"
	"github.com/pion/webrtc/v4"
)

const (
	testOldURL   = "wss://old"
	testOldToken = "old-token"
)

var errFakeConnect = errors.New("boom")

type fakeRoom struct {
	mu           sync.Mutex
	state        lksdk.ConnectionState
	published    [][]byte
	tracks       int
	unpublished  int
	disconnected int
}

func newFakeRoom() *fakeRoom {
	return &fakeRoom{state: lksdk.ConnectionStateConnected}
}

func (r *fakeRoom) publishData(data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.published = append(r.published, append([]byte(nil), data...))
	return nil
}

func (r *fakeRoom) publishTrack(webrtc.TrackLocal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tracks++
	return nil
}

func (r *fakeRoom) unpublishLocalTracks() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unpublished++
}

func (r *fakeRoom) disconnect() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disconnected++
	r.state = lksdk.ConnectionStateDisconnected
}

func (r *fakeRoom) connectionState() lksdk.ConnectionState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

type fakeConnector struct {
	mu        sync.Mutex
	urls      []string
	tokens    []string
	callbacks []*lksdk.RoomCallback
	rooms     []*fakeRoom
	connected chan struct{}
	err       error
}

func newFakeConnector() *fakeConnector {
	return &fakeConnector{connected: make(chan struct{}, 8)}
}

func (c *fakeConnector) connect(url, token string, cb *lksdk.RoomCallback) (roomHandle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	room := newFakeRoom()
	c.urls = append(c.urls, url)
	c.tokens = append(c.tokens, token)
	c.callbacks = append(c.callbacks, cb)
	c.rooms = append(c.rooms, room)
	c.connected <- struct{}{}
	return room, nil
}

func (c *fakeConnector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.rooms)
}

func (c *fakeConnector) callback(i int) *lksdk.RoomCallback {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.callbacks[i]
}

func (c *fakeConnector) room(i int) *fakeRoom {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rooms[i]
}

func (c *fakeConnector) snapshot() ([]string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.urls...), append([]string(nil), c.tokens...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

//nolint:cyclop // reconnect flow test keeps setup and postconditions in one scenario
func TestReconnectRefreshesCredentialsAndReplacesRoom(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refreshes := 0
	sess, err := New(ctx, engine.Config{
		URL:   testOldURL,
		Token: testOldToken,
		Refresh: func(context.Context) (engine.Credentials, error) {
			refreshes++
			return engine.Credentials{URL: "wss://new", Token: "new-token"}, nil
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	s, ok := sess.(*Session)
	if !ok {
		t.Fatalf("New() type = %T, want *Session", sess)
	}
	connector := newFakeConnector()
	s.connectRoom = connector.connect

	reconnected := make(chan struct{}, 1)
	s.SetReconnectCallback(func(*webrtc.DataChannel) {
		reconnected <- struct{}{}
	})

	if err := s.Connect(ctx); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	go s.WatchConnection(ctx)

	connector.callback(0).OnDisconnected()

	waitFor(t, func() bool { return connector.count() == 2 })
	select {
	case <-reconnected:
	case <-time.After(time.Second):
		t.Fatal("reconnect callback was not called")
	}

	urls, tokens := connector.snapshot()
	if got, want := urls, []string{testOldURL, "wss://new"}; !equalStrings(got, want) {
		t.Fatalf("connect urls = %v, want %v", got, want)
	}
	if got, want := tokens, []string{testOldToken, "new-token"}; !equalStrings(got, want) {
		t.Fatalf("connect tokens = %v, want %v", got, want)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
	oldRoom := connector.room(0)
	oldRoom.mu.Lock()
	if oldRoom.disconnected != 1 || oldRoom.unpublished != 1 {
		t.Fatalf("old room cleanup disconnected=%d unpublished=%d, want 1/1",
			oldRoom.disconnected, oldRoom.unpublished)
	}
	oldRoom.mu.Unlock()
	if !s.CanSend() {
		t.Fatal("CanSend() = false after reconnect, want true")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

//nolint:cyclop // terminal disconnect test keeps setup and cleanup assertions together
func TestDisconnectedEndsWhenReconnectDisallowed(t *testing.T) {
	ctx := context.Background()
	sess, err := New(ctx, engine.Config{URL: testOldURL, Token: testOldToken})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	s, ok := sess.(*Session)
	if !ok {
		t.Fatalf("New() type = %T, want *Session", sess)
	}
	connector := newFakeConnector()
	s.connectRoom = connector.connect
	s.SetShouldReconnect(func() bool { return false })

	ended := make(chan string, 1)
	s.SetEndedCallback(func(reason string) {
		ended <- reason
	})

	if err := s.Connect(ctx); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	connector.callback(0).OnDisconnected()

	select {
	case reason := <-ended:
		if reason != "disconnected from livekit" {
			t.Fatalf("ended reason = %q, want disconnected from livekit", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("ended callback was not called")
	}
	if !s.closed.Load() {
		t.Fatal("closed = false after terminal disconnect")
	}
	if connector.count() != 1 {
		t.Fatalf("connect count = %d, want 1", connector.count())
	}
	room := connector.room(0)
	room.mu.Lock()
	if room.disconnected != 1 || room.unpublished != 1 {
		t.Fatalf("terminal room cleanup disconnected=%d unpublished=%d, want 1/1",
			room.disconnected, room.unpublished)
	}
	room.mu.Unlock()

	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	room.mu.Lock()
	if room.disconnected != 1 || room.unpublished != 1 {
		t.Fatalf("second close cleanup disconnected=%d unpublished=%d, want still 1/1",
			room.disconnected, room.unpublished)
	}
	room.mu.Unlock()
}

func TestCanSendRequiresConnectedRoomAndQueueHeadroom(t *testing.T) {
	s := &Session{
		sendQueue: make(chan []byte, defaultSendQueueSize),
		done:      make(chan struct{}),
		closeCh:   make(chan struct{}),
	}
	if s.CanSend() {
		t.Fatal("CanSend() = true without room")
	}

	room := newFakeRoom()
	room.state = lksdk.ConnectionStateDisconnected
	s.setRoom(room)
	if s.CanSend() {
		t.Fatal("CanSend() = true for disconnected room")
	}

	room.state = lksdk.ConnectionStateConnected
	if !s.CanSend() {
		t.Fatal("CanSend() = false for connected room")
	}

	for range defaultSendQueueCapHard {
		s.sendQueue <- []byte("x")
	}
	if s.CanSend() {
		t.Fatal("CanSend() = true at queue high watermark")
	}
}

func TestReconnectFailureRetriesUntilContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Session{
		url:   testOldURL,
		token: testOldToken,
		connectRoom: func(string, string, *lksdk.RoomCallback) (roomHandle, error) {
			cancel()
			return nil, errFakeConnect
		},
		reconnectCh: make(chan struct{}, 1),
		closeCh:     make(chan struct{}),
		sendQueue:   make(chan []byte, defaultSendQueueSize),
		done:        make(chan struct{}),
	}
	if terminal := s.handleReconnectAttempt(ctx); !terminal {
		t.Fatal("handleReconnectAttempt() = false after context cancellation")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
