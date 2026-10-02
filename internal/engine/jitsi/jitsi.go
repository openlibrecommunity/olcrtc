// Package jitsi implements an engine.Session backed by Jitsi Meet's
// XMPP, Jingle, colibri-ws, SCTP, and WebRTC protocols.
//
// A session joins the MUC, waits for Jicofo's session-initiate after another
// participant arrives, opens the JVB bridge for bytes, and negotiates a pion
// PeerConnection when SCTP or video requires one. The auth provider parses the
// room URL and passes the host in engine.Config.URL and room in Extra["room"].
package jitsi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/zarazaex69/j"

	"github.com/openlibrecommunity/olcrtc/internal/engine"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

const (
	defaultNick       = "olcrtc"
	credentialKeyRoom = "room"
	maxReconnects     = 5
	// defaultSendQueueSize bounds what waits for the bridge, per destination.
	// At the ~5 Mbit/s a JVB relay carries, 32 frames of 16 KB is under a
	// second of queue. Control pings and pongs share this path with bulk
	// data, and a pong that waits longer than the liveness timeout is a
	// session torn down. 5000 was minutes of queue, and a full queue was an
	// error that closed smux at once (olcbox #15).
	defaultSendQueueSize = 32
	// peerQueueIdle is how long an untouched per-peer queue lives. Peer IDs
	// are data epochs, so a client that reconnects arrives under a new one;
	// on a server that runs for days the map would otherwise only grow.
	peerQueueIdle = time.Minute
)

var (
	// ErrSessionClosed is returned when an operation is attempted on a closed session.
	ErrSessionClosed = errors.New("jitsi session closed")
	// ErrBridgeNotReady is returned when send is attempted before the bridge is open.
	ErrBridgeNotReady = errors.New("jitsi bridge not ready")
	// ErrSendTooLarge is returned when a payload exceeds the JVB message limit.
	ErrSendTooLarge = errors.New("jitsi payload exceeds bridge max-message-size")
	// ErrHostRequired is returned when no Jitsi host was supplied.
	ErrHostRequired = errors.New("jitsi host required")
	// ErrRoomRequired is returned when no Jitsi room was supplied.
	ErrRoomRequired = errors.New("jitsi room required")
	// errNoPeer reports that a full reconnect joined the room without a peer.
	errNoPeer = errors.New("no peer in room")
)

// Session is the Jitsi engine handle.
type Session struct {
	engine.Reconnector
	engine.VideoTrackState

	host       string
	room       string
	name       string
	resolver   *net.Resolver
	httpClient *http.Client

	onData              func([]byte)
	onPeerData          func(peerID string, data []byte)
	requireTargetedPeer bool
	jSess               atomic.Pointer[j.Session]
	jSessMu             sync.Mutex
	jSessReady          chan struct{}

	pcMu          sync.Mutex
	pc            *webrtc.PeerConnection
	pcCtx         context.Context //nolint:containedctx // tied to PC lifetime
	pcCancel      context.CancelFunc
	trickleCancel context.CancelFunc

	sendQueue chan []byte
	// peerQueues holds one bounded queue per addressed peer, so a client that
	// cannot drain its share does not hold the room's other clients behind
	// it. peerWake is buffered(1): "some peer queue has data".
	peerQueueMu  sync.Mutex
	peerQueues   map[string]*peerQueue
	peerWake     chan struct{}
	bridgeReady  atomic.Bool
	bridgeGen    atomic.Uint64
	closed       atomic.Bool
	reconnecting atomic.Bool

	goMu sync.Mutex
	// recvMu admits one recvLoop at a time; see recvLoop.
	recvMu   sync.Mutex
	goClosed bool

	lastReconnectAt atomic.Int64
	localEpoch      atomic.Uint32
	peerEpoch       atomic.Uint32
	peerEndpoint    atomic.Pointer[string]
	peerEpochMu     sync.Mutex
	peerEpochs      map[string]uint32
	peerVideoSSRC   atomic.Uint32

	done     chan struct{}
	doneOnce sync.Once
	cancel   context.CancelFunc
	runCtx   context.Context //nolint:containedctx // engine owns supervisor lifetime
	wg       sync.WaitGroup
}

// peerQueue is one peer's share of the bridge, with the bookkeeping that
// lets it be dropped safely: refs counts senders that hold the channel and
// may still push into it, lastUsed is when it last took a frame.
type peerQueue struct {
	ch       chan []byte
	refs     int
	lastUsed time.Time
}

// New creates a Jitsi engine session.
func New(_ context.Context, cfg engine.Config) (engine.Session, error) {
	host := normaliseHost(cfg.URL)
	if host == "" {
		return nil, ErrHostRequired
	}
	var room string
	if cfg.Extra != nil {
		room = strings.TrimSpace(cfg.Extra[credentialKeyRoom])
	}
	if room == "" {
		return nil, ErrRoomRequired
	}
	name := sanitiseNick(cfg.Name)
	if name == "" {
		name = defaultNick
	}

	runCtx, cancel := context.WithCancel(context.Background())
	s := &Session{
		host:                host,
		room:                room,
		name:                name,
		resolver:            cfg.Resolver,
		httpClient:          protect.NewHTTPClient(cfg.Resolver),
		onData:              cfg.OnData,
		onPeerData:          cfg.OnPeerData,
		requireTargetedPeer: cfg.RequireTargetedPeer,
		sendQueue:           make(chan []byte, defaultSendQueueSize),
		peerQueues:          make(map[string]*peerQueue),
		peerWake:            make(chan struct{}, 1),
		peerEpochs:          make(map[string]uint32),
		jSessReady:          make(chan struct{}),
		done:                make(chan struct{}),
		cancel:              cancel,
		runCtx:              runCtx,
	}
	s.Configure(engine.ReconnectorConfig{
		MaxAttempts:   maxReconnects,
		CountFailures: true,
		Reconnect: func(ctx context.Context) error {
			err := s.reconnect(ctx)
			if errors.Is(err, errNoPeer) {
				logger.Infof("jitsi: waiting for peer in room (not a failure)")
			}
			return err
		},
		IsNonFailure: func(err error) bool {
			return errors.Is(err, errNoPeer)
		},
		OnError: func(err error) {
			logger.Warnf("jitsi reconnect failed: %v", err)
		},
		OnLimit:     s.signalEnded,
		LimitReason: "jitsi reconnect limit reached",
	})
	s.localEpoch.Store(randomEpoch())
	return s, nil
}

// goLaunch starts a tracked goroutine unless Close has started waiting.
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

func (s *Session) stopLaunching() {
	s.goMu.Lock()
	s.goClosed = true
	s.goMu.Unlock()
}

// Connect joins the MUC and waits for Jicofo asynchronously because Jicofo
// only sends session-initiate after another participant joins.
func (s *Session) Connect(ctx context.Context) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}

	logger.Infof("jitsi: joining MUC %s/%s as %s …", s.host, s.room, s.name)
	jSess, err := j.JoinMUC(ctx, j.Config{
		Host:       s.host,
		Room:       s.room,
		Nick:       s.name,
		Debug:      logger.IsVerbose(),
		HTTPClient: s.httpClient,
	})
	if err != nil {
		return fmt.Errorf("jitsi join muc: %w", err)
	}
	s.setJSession(jSess)
	logger.Infof("jitsi: MUC joined %s/%s; waiting for peer …", s.host, s.room)

	s.goLaunch(s.sendLoop)
	s.goLaunch(s.recvLoop)
	s.goLaunch(s.waitForJingle)
	s.goLaunch(s.bridgeKeepalive)
	s.goLaunch(s.xmppKeepalive)
	return nil
}

// Close terminates media before leaving the MUC, matching Jitsi's graceful
// leave order and preventing new tracked goroutines before Wait starts.
func (s *Session) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}

	jSess := s.jSess.Load()
	s.pcMu.Lock()
	pc := s.pc
	s.pc = nil
	pcCancel := s.pcCancel
	s.pcCancel = nil
	s.pcCtx = nil
	s.pcMu.Unlock()
	if pcCancel != nil {
		pcCancel()
	}
	if pc != nil {
		_ = pc.Close()
	}
	if jSess != nil {
		_ = jSess.Close()
	}
	s.setJSession(nil)
	s.bridgeReady.Store(false)

	if s.cancel != nil {
		s.cancel()
	}
	s.doneOnce.Do(func() { close(s.done) })
	s.stopLaunching()

	stopped := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
	}
	return nil
}

// ResetPeer clears endpoint and epoch binding after an upper-layer handshake failure.
func (s *Session) ResetPeer() {
	s.peerEndpoint.Store(nil)
	s.peerEpoch.Store(0)
	s.resetPeerEpochs()
}

func (s *Session) notifyReconnect() {
	s.NotifyReconnect()
}

func (s *Session) reconnectAllowed() bool {
	return s.ShouldReconnect()
}

// WatchConnection services reconnect requests until the session ends.
func (s *Session) WatchConnection(ctx context.Context) {
	s.Watch(ctx, s.done)
}

// Reconnect requests a bridge rebuild from the shared supervisor.
func (s *Session) Reconnect(reason string) {
	s.requestReconnect(reason)
}

// CanSend reports whether the configured byte or video path is ready.
func (s *Session) CanSend() bool {
	if s.closed.Load() {
		return false
	}
	if s.onData == nil && s.onPeerData == nil {
		s.pcMu.Lock()
		ready := s.pc != nil && s.pc.ConnectionState() == webrtc.PeerConnectionStateConnected
		s.pcMu.Unlock()
		return ready
	}
	return s.bridgeReady.Load()
}

// SubscriberCanSend reports whether the subscriber path is ready to send.
func (s *Session) SubscriberCanSend() bool {
	return s.CanSend()
}

// GetBufferedAmount estimates bytes pending in the bridge's message queue.
func (s *Session) GetBufferedAmount() uint64 {
	jSess := s.jSess.Load()
	if jSess == nil {
		return 0
	}
	depth := jSess.BridgeSendQueueDepth()
	if depth <= 0 {
		return 0
	}
	return uint64(depth) * uint64(bridgeMaxMessageSize)
}

// AddVideoTrack publishes a track immediately when a PC is live, or stores it
// for the next negotiation otherwise.
func (s *Session) AddVideoTrack(track webrtc.TrackLocal) error {
	s.StoreVideoTrack(track)

	s.pcMu.Lock()
	pc := s.pc
	s.pcMu.Unlock()
	if pc == nil {
		return nil
	}
	if _, err := pc.AddTrack(track); err != nil {
		return fmt.Errorf("add track: %w", err)
	}
	return nil
}

func (s *Session) signalEnded(reason string) {
	s.bridgeReady.Store(false)
	s.SignalEnded(reason)
}
