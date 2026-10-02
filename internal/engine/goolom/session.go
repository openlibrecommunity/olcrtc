// Package goolom implements an engine.Session backed by the Goolom SFU
// signaling protocol. Goolom is the proprietary SFU developed for Yandex
// Telemost; the on-wire protocol - capabilities offer, separated subscriber
// and publisher PeerConnections, ack/pong keepalive, slots-based subscribe
// model - is what this engine speaks.
//
// HTTP auth (room-info lookup, telemetry referer, etc.) lives in the auth
// package; this engine consumes a media-server WebSocket URL plus the
// peer/room/credentials tuple supplied as engine.Config.
package goolom

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/openlibrecommunity/olcrtc/internal/engine"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

const (
	realDataChannelMessageLimit = 12288
	defaultSendDelayLow         = 2 * time.Millisecond
	defaultSendDelayMax         = 12 * time.Millisecond
	defaultTelemetryInterval    = 20 * time.Second
	// minTelemetryInterval and maxTelemetryInterval bound the cadence the
	// media server asks for.
	minTelemetryInterval       = time.Second
	maxTelemetryInterval       = 5 * time.Minute
	defaultBufferHighWaterMark = 512 * 1024

	wsReadTimeout      = 60 * time.Second
	wsWriteTimeout     = 15 * time.Second
	wsHandshakeTimeout = 15 * time.Second
	// wsReadLimit caps one signaling frame. gorilla reads without a limit by
	// default, so the media server could otherwise name any size and have it
	// buffered and JSON-decoded in full.
	wsReadLimit = 8 << 20

	keyUID          = "uid"
	keyDescription  = "description"
	keyPcSeq        = "pcSeq"
	keyName         = "name"
	stateTerminated = "terminated"

	credentialKeyRoomID           = "roomID"
	credentialKeyCredentials      = "credentials"
	credentialKeyRoomURL          = "roomURL"
	credentialKeyTelemetryReferer = "telemetryReferer"
)

var (
	// ErrDataChannelTimeout is returned when the DataChannel fails to open in time.
	ErrDataChannelTimeout = errors.New("datachannel timeout")
	// ErrDataChannelNotReady is returned when send is called before the DataChannel is open.
	ErrDataChannelNotReady = errors.New("datachannel not ready")
	// ErrSendQueueClosed is returned when send is called after Close.
	ErrSendQueueClosed = errors.New("send queue closed")
	// ErrSendQueueTimeout is returned when the send queue cannot accept new data in time.
	ErrSendQueueTimeout = errors.New("send queue timeout")
	// ErrSessionClosed is returned when the session is closed mid-operation.
	ErrSessionClosed = errors.New("session closed")
	// ErrPeerClosed is returned when the peer is closed mid-operation.
	ErrPeerClosed = errors.New("peer closed")
	// ErrSubscriberMediaTimeout is returned when the subscriber media is not ready in time.
	ErrSubscriberMediaTimeout = errors.New("subscriber media timeout")
	// ErrPublisherNotInitialized is returned when the publisher PC is not set up.
	ErrPublisherNotInitialized = errors.New("publisher peer connection not initialized")
	// ErrURLRequired is returned when no media-server WebSocket URL was supplied.
	ErrURLRequired = errors.New("goolom media server URL required")
	// ErrRoomIDRequired is returned when no room ID was supplied.
	ErrRoomIDRequired = errors.New("goolom room ID required")
	// ErrPeerIDRequired is returned when no peer ID was supplied.
	ErrPeerIDRequired = errors.New("goolom peer ID required")
	// ErrNoRefresh is returned when reconnect is attempted without a refresh callback.
	ErrNoRefresh = errors.New("goolom reconnect: no refresh callback supplied")
	// ErrWebSocketClosed is returned when a signaling write is attempted
	// while no WebSocket is connected.
	ErrWebSocketClosed = errors.New("goolom signaling websocket closed")
)

// TrafficShape controls outgoing data-channel pacing.
type TrafficShape struct {
	MaxMessageSize int
	MinDelay       time.Duration
	MaxDelay       time.Duration
}

// Session is the Goolom engine handle.
type Session struct {
	engine.Reconnector
	engine.VideoTrackState

	name             string
	mediaServerURL   string
	peerID           string
	roomID           string
	credentials      string
	roomURL          string // referer for telemetry - opaque to the engine
	telemetryReferer string
	refresh          func(ctx context.Context) (engine.Credentials, error)
	resolver         protect.Lookup

	// wsMu owns ws: it serialises the writes (gorilla allows a single
	// concurrent writer) and guards the pointer itself, which Connect
	// replaces on every reconnect.
	wsMu sync.Mutex
	ws   *websocket.Conn

	// The peer connections and the data channel are replaced on every
	// reconnect while pion callbacks from the previous generation are still
	// running, so they are published atomically.
	pcSub atomic.Pointer[webrtc.PeerConnection]
	pcPub atomic.Pointer[webrtc.PeerConnection]
	dc    atomic.Pointer[webrtc.DataChannel]

	onData func([]byte)

	closeCh        chan struct{}
	closeOnce      sync.Once
	keepAliveCh    chan struct{}
	telemetryCh    chan struct{}
	sessionCloseCh chan struct{}
	sessionMu      sync.Mutex

	sendQueue       chan []byte
	sendQueueClosed atomic.Bool
	closed          atomic.Bool
	// terminated is the one-way flag Close sets. closed is cleared again by
	// Connect on every reconnect, so it cannot answer "is this session gone
	// for good?" - which is what the reconnect path has to ask before it
	// builds a new PeerConnection, DataChannel and WebSocket that Close has
	// already finished tearing down.
	terminated atomic.Bool
	// goMu guards goClosed, which stops new tracked goroutines once Close has
	// started waiting on wg.
	goMu     sync.Mutex
	goClosed bool
	// credMu guards the credential fields below. A reconnect rewrites them
	// while the previous session's telemetry goroutine is still reading them:
	// stopTelemetry is a best-effort signal and nothing waits for that
	// goroutine to leave.
	credMu          sync.RWMutex
	reconnecting    atomic.Bool
	telemetryActive atomic.Bool

	ackMu      sync.Mutex
	ackWaiters map[string]chan struct{}

	trafficShape TrafficShape

	subscriberReady atomic.Bool
	publisherReady  atomic.Bool

	// mediaMu guards subscriberConn, which Connect replaces while the
	// callbacks of the previous subscriber PC may still be signalling it.
	mediaMu        sync.Mutex
	subscriberConn chan struct{}

	wg sync.WaitGroup
}

// wsConn returns the live signaling connection, or nil when there is none.
func (s *Session) wsConn() *websocket.Conn {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	return s.ws
}

// writeJSON serialises v onto the signaling WebSocket. It is the single
// writer path: it owns wsMu (gorilla permits one concurrent writer) and it is
// the only place that has to deal with a torn-down connection.
//
// The deadline is what keeps wsMu a lock and not a trap. gorilla has no write
// deadline by default, so on a black-holed socket the write blocks forever
// with wsMu held - taking every other writer with it, including the keepalive
// that would have detected the dead link and the Close that would have torn
// it down.
func (s *Session) writeJSON(v any) error {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	if s.ws == nil {
		return ErrWebSocketClosed
	}
	_ = s.ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	if err := s.ws.WriteJSON(v); err != nil {
		return fmt.Errorf("ws write: %w", err)
	}
	return nil
}

// goLaunch starts a tracked goroutine unless Close has started waiting.
// sync.WaitGroup forbids a positive Add from a zero counter concurrent with
// Wait, and the signaling, telemetry and data-channel paths all spawn from
// goroutines Close does not control.
func (s *Session) goLaunch(fn func()) {
	s.goMu.Lock()
	if s.goClosed {
		s.goMu.Unlock()
		return
	}
	s.wg.Add(1)
	s.goMu.Unlock()

	go func() {
		defer s.wg.Done()
		fn()
	}()
}

// credentialSnapshot returns the identity fields (peer ID, room ID, referer)
// under one read lock so a concurrent refresh cannot tear them apart.
func (s *Session) credentialSnapshot() (string, string, string) {
	s.credMu.RLock()
	defer s.credMu.RUnlock()
	return s.peerID, s.roomID, s.telemetryReferer
}

// joinCredentials returns the peer ID, room ID and credentials the join
// message carries.
func (s *Session) joinCredentials() (string, string, string) {
	s.credMu.RLock()
	defer s.credMu.RUnlock()
	return s.peerID, s.roomID, s.credentials
}

// signalingURL returns the media server endpoint to dial.
func (s *Session) signalingURL() string {
	s.credMu.RLock()
	defer s.credMu.RUnlock()
	return s.mediaServerURL
}

func (s *Session) stopLaunching() {
	s.goMu.Lock()
	s.goClosed = true
	s.goMu.Unlock()
}

// subPC returns the live subscriber PeerConnection, or nil before setup.
func (s *Session) subPC() *webrtc.PeerConnection { return s.pcSub.Load() }

// pubPC returns the live publisher PeerConnection, or nil before setup.
func (s *Session) pubPC() *webrtc.PeerConnection { return s.pcPub.Load() }

// dataChannel returns the live data channel, or nil when the session carries
// no byte stream.
func (s *Session) dataChannel() *webrtc.DataChannel { return s.dc.Load() }

// signalSubscriberConn reports that subscriber media is flowing. Serialised
// so a callback from a superseded PC cannot close an already-closed channel.
func (s *Session) signalSubscriberConn() {
	s.mediaMu.Lock()
	engine.CloseSignal(s.subscriberConn)
	s.mediaMu.Unlock()
}

// subscriberConnCh returns the current subscriber-ready signal.
func (s *Session) subscriberConnCh() <-chan struct{} {
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	return s.subscriberConn
}

// New creates a new Goolom engine session.
//
// cfg.URL is the media server WebSocket URL. cfg.Token carries the peer ID.
// cfg.Extra carries the rest of the room tuple: roomID, credentials, and an
// optional roomURL / telemetryReferer string the engine uses verbatim as the
// Referer header for telemetry posts.
func New(_ context.Context, cfg engine.Config) (engine.Session, error) {
	if cfg.URL == "" {
		return nil, ErrURLRequired
	}
	peerID := cfg.Token
	if peerID == "" {
		return nil, ErrPeerIDRequired
	}
	roomID := ""
	credentials := ""
	roomURL := ""
	telemetryReferer := ""
	if cfg.Extra != nil {
		roomID = cfg.Extra[credentialKeyRoomID]
		credentials = cfg.Extra[credentialKeyCredentials]
		roomURL = cfg.Extra[credentialKeyRoomURL]
		telemetryReferer = cfg.Extra[credentialKeyTelemetryReferer]
	}
	if roomID == "" {
		return nil, ErrRoomIDRequired
	}
	if telemetryReferer == "" {
		telemetryReferer = roomURL
	}

	s := &Session{
		name:             cfg.Name,
		mediaServerURL:   cfg.URL,
		peerID:           peerID,
		roomID:           roomID,
		credentials:      credentials,
		roomURL:          roomURL,
		telemetryReferer: telemetryReferer,
		refresh:          cfg.Refresh,
		resolver:         cfg.Resolver,
		onData:           cfg.OnData,
		closeCh:          make(chan struct{}),
		keepAliveCh:      make(chan struct{}),
		sessionCloseCh:   make(chan struct{}),
		telemetryCh:      make(chan struct{}, 1),
		sendQueue:        make(chan []byte, engine.DefaultSendQueueSize),
		ackWaiters:       make(map[string]chan struct{}),
		subscriberConn:   make(chan struct{}),
		trafficShape: TrafficShape{
			MaxMessageSize: realDataChannelMessageLimit,
			MinDelay:       defaultSendDelayLow,
			MaxDelay:       defaultSendDelayMax,
		},
	}
	s.Configure(engine.ReconnectorConfig{
		MaxAttempts: 10,
		Reconnect:   s.reconnect,
		OnError: func(err error) {
			logger.Debugf("reconnect failed: %v", err)
		},
		OnLimit:     s.signalEnded,
		LimitReason: "reconnect limit reached",
	})
	return s, nil
}

// SetTrafficShape adjusts the outgoing data-channel pacing.
func (s *Session) SetTrafficShape(shape TrafficShape) {
	if shape.MaxMessageSize <= 0 {
		shape.MaxMessageSize = realDataChannelMessageLimit
	}
	if shape.MaxDelay < shape.MinDelay {
		shape.MaxDelay = shape.MinDelay
	}
	s.trafficShape = shape
}

// Send queues data for transmission.
func (s *Session) Send(data []byte) error {
	if dc := s.dataChannel(); dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return ErrDataChannelNotReady
	}
	if s.sendQueueClosed.Load() {
		return ErrSendQueueClosed
	}
	select {
	case s.sendQueue <- data:
		return nil
	case <-time.After(50 * time.Millisecond):
		return ErrSendQueueTimeout
	}
}

// GetBufferedAmount returns the WebRTC buffered amount.
func (s *Session) GetBufferedAmount() uint64 {
	if dc := s.dataChannel(); dc != nil {
		return dc.BufferedAmount()
	}
	return 0
}

// SubscriberCanSend reports whether the subscriber PC is connected.
// Unlike CanSend, it does not require publisherReady, so it returns true
// as soon as SFU data can arrive - before the publisher PC negotiates.
func (s *Session) SubscriberCanSend() bool {
	return !s.closed.Load() && s.subscriberReady.Load()
}

// CanSend checks if data can be sent.
func (s *Session) CanSend() bool {
	if s.onData == nil {
		// publisherReady is intentionally not checked: KCP buffers outbound
		// data and retransmits if WriteSample fails while the publisher PC is
		// still connecting. Blocking on publisherReady causes handshake welcome
		// and tunnel stream acks to time out before the publisher PC is up.
		return !s.closed.Load() && s.subscriberReady.Load()
	}
	if dc := s.dataChannel(); dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return false
	}
	return len(s.sendQueue) < engine.DefaultSendQueueCapHard
}

// AddVideoTrack adds a video track to the publisher peer connection.
func (s *Session) AddVideoTrack(track webrtc.TrackLocal) error {
	s.StoreVideoTrack(track)

	pub := s.pubPC()
	if pub == nil {
		return nil
	}
	if _, err := pub.AddTrack(track); err != nil {
		return fmt.Errorf("failed to add track: %w", err)
	}
	return nil
}

func (s *Session) hasLocalVideoTracks() bool {
	return s.HasVideoTracks()
}

func (s *Session) videoTrackHandler() func(*webrtc.TrackRemote, *webrtc.RTPReceiver) {
	return s.VideoTrackHandler()
}

func (s *Session) attachPendingVideoTracks(pub *webrtc.PeerConnection) error {
	var attachErr error
	s.RangeVideoTracks(func(track webrtc.TrackLocal, _ bool) {
		if attachErr != nil {
			return
		}
		sender, err := pub.AddTrack(track)
		if err != nil {
			attachErr = fmt.Errorf("add video track: %w", err)
			return
		}
		s.drainPublisherRTCP(sender)
	})
	return attachErr
}

// drainPublisherRTCP reads (and discards) RTCP feedback the SFU sends for our
// published track. The read is required so the interceptor chain keeps
// processing incoming RTCP; without an active reader it stalls.
func (s *Session) drainPublisherRTCP(sender *webrtc.RTPSender) {
	if sender == nil {
		return
	}
	go engine.DrainRTCP(sender)
}
