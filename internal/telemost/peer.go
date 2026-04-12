package telemost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
	"github.com/pion/webrtc/v4"
)

const (
	realDataChannelMessageLimit = 8192
	defaultSendDelayMin         = 2 * time.Millisecond
	defaultSendDelayMax         = 12 * time.Millisecond
	defaultTelemetryInterval    = 20 * time.Second
)

var (
	// ErrDataChannelTimeout is returned when the datachannel fails to open within the timeout.
	ErrDataChannelTimeout = errors.New("datachannel timeout")
	// ErrDataChannelNotReady is returned when the datachannel is not open.
	ErrDataChannelNotReady = errors.New("datachannel not ready")
	// ErrSendQueueClosed is returned when the send queue is closed.
	ErrSendQueueClosed = errors.New("send queue closed")
	// ErrSendQueueTimeout is returned when sending to the queue times out.
	ErrSendQueueTimeout = errors.New("send queue timeout")
	// ErrSessionClosed is returned when the session is closed.
	ErrSessionClosed = errors.New("session closed")
	// ErrPeerClosed is returned when the peer is closed.
	ErrPeerClosed = errors.New("peer closed")
)

type TrafficShape struct { //nolint:revive
	MaxMessageSize int
	MinDelay       time.Duration
	MaxDelay       time.Duration
}

type Peer struct { //nolint:revive
	roomURL         string
	name            string
	conn            *ConnectionInfo
	ws              *websocket.Conn
	wsMu            sync.Mutex
	pcSub           *webrtc.PeerConnection
	pcPub           *webrtc.PeerConnection
	dc              *webrtc.DataChannel
	onData          func([]byte)
	onReconnect     func(*webrtc.DataChannel)
	shouldReconnect func() bool
	reconnectCh     chan struct{}
	closeCh         chan struct{}
	keepAliveCh     chan struct{}
	telemetryCh     chan struct{}
	lastReconnect   time.Time
	reconnectCount  int
	sessionMu       sync.Mutex
	sendQueue       chan []byte
	sendQueueClosed atomic.Bool
	closed          atomic.Bool
	reconnecting    atomic.Bool
	telemetryActive atomic.Bool
	ackMu           sync.Mutex
	ackWaiters      map[string]chan struct{}
	onEnded         func(string)
	trafficShape    TrafficShape
	sessionCloseCh  chan struct{}
	wg              sync.WaitGroup
}

func (p *Peer) GetSendQueue() chan []byte { //nolint:revive
	return p.sendQueue
}

func (p *Peer) GetBufferedAmount() uint64 { //nolint:revive
	if p.dc != nil {
		return p.dc.BufferedAmount()
	}
	return 0
}

func (p *Peer) SetEndedCallback(cb func(string)) { //nolint:revive
	p.onEnded = cb
}

func (p *Peer) SetTrafficShape(shape TrafficShape) { //nolint:revive
	if shape.MaxMessageSize <= 0 {
		shape.MaxMessageSize = realDataChannelMessageLimit
	}
	if shape.MaxDelay < shape.MinDelay {
		shape.MaxDelay = shape.MinDelay
	}
	p.trafficShape = shape
}

func NewPeer(ctx context.Context, roomURL, name string, onData func([]byte)) (*Peer, error) { //nolint:revive
	conn, err := GetConnectionInfo(ctx, roomURL, name)
	if err != nil {
		return nil, fmt.Errorf("failed to get connection info: %w", err)
	}

	return &Peer{
		roomURL:        roomURL,
		name:           name,
		conn:           conn,
		onData:         onData,
		reconnectCh:    make(chan struct{}, 1),
		closeCh:        make(chan struct{}),
		keepAliveCh:    make(chan struct{}),
		sessionCloseCh: make(chan struct{}),
		telemetryCh:    make(chan struct{}, 1),
		sendQueue:      make(chan []byte, 5000),
		ackWaiters:     make(map[string]chan struct{}),
		trafficShape: TrafficShape{
			MaxMessageSize: realDataChannelMessageLimit,
			MinDelay:       defaultSendDelayMin,
			MaxDelay:       defaultSendDelayMax,
		},
	}, nil
}

func closeSignal(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (p *Peer) queueReconnect() {
	if p.closed.Load() || p.reconnecting.Load() {
		return
	}
	if p.shouldReconnect != nil && !p.shouldReconnect() {
		log.Println("Reconnect skipped: shouldReconnect returned false")
		return
	}
	select {
	case p.reconnectCh <- struct{}{}:
	default:
	}
}

func (p *Peer) stopSession() {
	p.stopTelemetry()

	p.sessionMu.Lock()
	closeSignal(p.keepAliveCh)
	closeSignal(p.sessionCloseCh)
	p.sessionMu.Unlock()
}

func (p *Peer) resetSession() (chan struct{}, chan struct{}) {
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()

	p.keepAliveCh = make(chan struct{})
	p.sessionCloseCh = make(chan struct{})
	return p.keepAliveCh, p.sessionCloseCh
}

func (p *Peer) drainReconnectQueue() {
	for {
		select {
		case <-p.reconnectCh:
		default:
			return
		}
	}
}

func (p *Peer) Connect(ctx context.Context) error { //nolint:revive
	p.closed.Store(false)

	config := webrtc.Configuration{
		ICEServers:   []webrtc.ICEServer{{URLs: []string{"stun:stun.rtc.yandex.net:3478"}}},
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	}

	if err := p.setupPeerConnections(config); err != nil {
		return err
	}

	var err error
	p.dc, err = p.pcPub.CreateDataChannel("olcrtc", nil)
	if err != nil {
		return fmt.Errorf("create dc: %w", err)
	}

	dcReady := make(chan struct{})
	keepAliveCh, sessionCloseCh := p.resetSession()
	p.setupDataChannelHandlers(dcReady, sessionCloseCh)

	if err := p.dialWebSocket(); err != nil {
		return err
	}

	p.setupICEHandlers()
	p.startBackgroundGoroutines(ctx, keepAliveCh)

	select {
	case <-dcReady:
		return nil
	case <-time.After(15 * time.Second):
		return ErrDataChannelTimeout
	case <-ctx.Done():
		return fmt.Errorf("connect context cancelled: %w", ctx.Err())
	}
}

func (p *Peer) setupPeerConnections(config webrtc.Configuration) error {
	settingEngine := webrtc.SettingEngine{}
	if protect.Protector != nil {
		settingEngine.SetICEProxyDialer(protect.NewProxyDialer())
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine))

	var err error
	p.pcSub, err = api.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("new sub pc: %w", err)
	}
	p.pcSub.OnConnectionStateChange(p.onConnectionStateChange)

	p.pcPub, err = api.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("new pub pc: %w", err)
	}
	p.pcPub.OnConnectionStateChange(p.onConnectionStateChange)

	return nil
}

func (p *Peer) onConnectionStateChange(state webrtc.PeerConnectionState) {
	log.Printf("PeerConnection state: %s", state.String())
	if !p.closed.Load() && (state == webrtc.PeerConnectionStateFailed ||
		state == webrtc.PeerConnectionStateDisconnected) {
		p.queueReconnect()
	}
}

func (p *Peer) setupDataChannelHandlers(dcReady chan struct{}, sessionCloseCh chan struct{}) {
	p.dc.OnOpen(func() {
		log.Println("DataChannel opened")
		numWorkers := 4
		for i := range numWorkers {
			p.wg.Add(1)
			go func(workerID int) {
				defer p.wg.Done()
				p.processSendQueue(workerID, sessionCloseCh)
			}(i)
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.monitorQueue(sessionCloseCh)
		}()
		close(dcReady)
	})

	p.dc.OnClose(p.onDataChannelClose)
	p.dc.OnMessage(p.onDataChannelMessage)

	p.pcSub.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("Received datachannel: %s", dc.Label())
		dc.OnClose(func() {
			if !p.closed.Load() {
				p.queueReconnect()
			}
		})
		dc.OnMessage(p.onDataChannelMessage)
	})
}

func (p *Peer) onDataChannelClose() {
	log.Println("DataChannel closed")
	if p.onReconnect != nil {
		p.onReconnect(nil)
	}
	if !p.closed.Load() {
		p.queueReconnect()
	}
}

func (p *Peer) onDataChannelMessage(msg webrtc.DataChannelMessage) {
	if p.onData != nil && len(msg.Data) > 0 {
		p.onData(msg.Data)
	}
}

func (p *Peer) dialWebSocket() error {
	wsDialer := websocket.Dialer{
		NetDialContext:   protect.DialContext,
		HandshakeTimeout: 15 * time.Second,
	}
	ws, resp, err := wsDialer.Dial(p.conn.ClientConfig.MediaServerURL, nil)
	if err != nil {
		return fmt.Errorf("dial ws: %w", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	p.ws = ws

	ws.SetPongHandler(func(string) error {
		_ = ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	_ = ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	return nil
}

func (p *Peer) startBackgroundGoroutines(ctx context.Context, keepAliveCh chan struct{}) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.keepAlive(keepAliveCh)
	}()

	_ = p.sendHello()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.handleSignaling(ctx)
	}()
}

func (p *Peer) Send(data []byte) error { //nolint:revive
	if p.dc == nil || p.dc.ReadyState() != webrtc.DataChannelStateOpen {
		return ErrDataChannelNotReady
	}

	if p.sendQueueClosed.Load() {
		return ErrSendQueueClosed
	}

	select {
	case p.sendQueue <- data:
		return nil
	case <-time.After(50 * time.Millisecond):
		queueLen := len(p.sendQueue)
		log.Printf("[SEND_QUEUE] Timeout! len=%d size=%d", queueLen, len(data))
		return ErrSendQueueTimeout
	}
}

func (p *Peer) sendHello() error {
	hello := map[string]interface{}{
		"uid": uuid.New().String(),
		"hello": map[string]interface{}{
			"participantMeta": map[string]interface{}{
				"name":      p.name,
				"role":      "SPEAKER",
				"sendAudio": false,
				"sendVideo": false,
			},
			"participantAttributes": map[string]interface{}{
				"name": p.name,
				"role": "SPEAKER",
			},
			"sendAudio":     false,
			"sendVideo":     false,
			"sendSharing":   false,
			"participantId": p.conn.PeerID,
			"roomId":        p.conn.RoomID,
			"serviceName":   "telemost",
			"credentials":   p.conn.Credentials,
			"capabilitiesOffer": map[string]interface{}{
				"offerAnswerMode":        []string{"SEPARATE"},
				"initialSubscriberOffer": []string{"ON_HELLO"},
				"slotsMode":              []string{"FROM_CONTROLLER"},
				"simulcastMode":          []string{"DISABLED"},
				"selfVadStatus":          []string{"FROM_SERVER"},
				"dataChannelSharing":     []string{"TO_RTP"},
			},
			"sdkInfo": map[string]interface{}{
				"implementation": "go",
				"version":        "1.0.0",
				"userAgent":      "OlcRTC-" + p.name,
			},
			"sdkInitializationId": uuid.New().String(),
			"disablePublisher":    false,
			"disableSubscriber":   false,
		},
	}

	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	if err := p.ws.WriteJSON(hello); err != nil {
		return fmt.Errorf("write hello: %w", err)
	}
	return nil
}

func (p *Peer) handleSignaling(ctx context.Context) { //nolint:cyclop
	pubSent := false

	for {
		var msg map[string]interface{}
		if err := p.ws.ReadJSON(&msg); err != nil {
			log.Printf("WS read error: %v", err)
			if !p.closed.Load() {
				p.queueReconnect()
			}
			return
		}

		p.updateWSDeadline()

		uid, _ := msg["uid"].(string)
		if _, ok := msg["ack"]; ok {
			p.resolveAck(uid)
		}

		if serverHello, ok := msg["serverHello"].(map[string]interface{}); ok {
			p.startTelemetry(ctx, serverHello)
			p.sendAck(uid)
		}

		p.handleCommonMessages(msg, uid)

		if isConferenceEndMessage(msg) {
			p.signalEnded("conference ended")
			return
		}

		if offer, ok := msg["subscriberSdpOffer"].(map[string]interface{}); ok && !pubSent {
			if err := p.handleSdpOffer(offer, uid); err != nil {
				log.Printf("SDP offer error: %v", err)
				continue
			}
			pubSent = true
		}

		if answer, ok := msg["publisherSdpAnswer"].(map[string]interface{}); ok {
			p.handleSdpAnswer(answer, uid)
		}

		if cand, ok := msg["webrtcIceCandidate"].(map[string]interface{}); ok {
			p.handleICE(cand)
		}
	}
}

func (p *Peer) updateWSDeadline() {
	p.wsMu.Lock()
	if p.ws != nil {
		_ = p.ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	}
	p.wsMu.Unlock()
}

func (p *Peer) handleCommonMessages(msg map[string]interface{}, uid string) {
	if _, ok := msg["updateDescription"]; ok {
		p.sendAck(uid)
	}
	if _, ok := msg["vadActivity"]; ok {
		p.sendAck(uid)
	}
	if _, ok := msg["ping"]; ok {
		p.sendPong(uid)
	}
	if _, ok := msg["pong"]; ok {
		p.sendAck(uid)
	}
}

func (p *Peer) handleSdpOffer(offer map[string]interface{}, uid string) error {
	sdp, _ := offer["sdp"].(string)
	pcSeq, _ := offer["pcSeq"].(float64)

	if err := p.pcSub.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}); err != nil {
		return fmt.Errorf("set remote desc: %w", err)
	}

	answer, err := p.pcSub.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create answer: %w", err)
	}

	if err := p.pcSub.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("set local desc: %w", err)
	}

	p.wsMu.Lock()
	_ = p.ws.WriteJSON(map[string]interface{}{
		"uid": uuid.New().String(),
		"subscriberSdpAnswer": map[string]interface{}{
			"pcSeq": int(pcSeq),
			"sdp":   answer.SDP,
		},
	})
	p.wsMu.Unlock()

	p.sendAck(uid)
	time.Sleep(300 * time.Millisecond)

	pubOffer, err := p.pcPub.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create pub offer: %w", err)
	}

	if err := p.pcPub.SetLocalDescription(pubOffer); err != nil {
		return fmt.Errorf("set local pub desc: %w", err)
	}

	p.wsMu.Lock()
	_ = p.ws.WriteJSON(map[string]interface{}{
		"uid": uuid.New().String(),
		"publisherSdpOffer": map[string]interface{}{
			"pcSeq": 1,
			"sdp":   pubOffer.SDP,
		},
	})
	p.wsMu.Unlock()
	return nil
}

func (p *Peer) handleSdpAnswer(answer map[string]interface{}, uid string) {
	sdp, _ := answer["sdp"].(string)
	if err := p.pcPub.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	}); err != nil {
		log.Printf("SetRemoteDescription error: %v", err)
	}
	p.sendAck(uid)
}

func (p *Peer) handleICE(cand map[string]interface{}) {
	candStr, _ := cand["candidate"].(string)
	target, _ := cand["target"].(string)
	sdpMid, _ := cand["sdpMid"].(string)
	sdpMLineIndex, _ := cand["sdpMlineIndex"].(float64)

	parts := strings.Fields(candStr)
	if len(parts) < 8 {
		return
	}

	init := webrtc.ICECandidateInit{
		Candidate:     candStr,
		SDPMid:        &sdpMid,
		SDPMLineIndex: func() *uint16 { v := uint16(sdpMLineIndex); return &v }(),
	}

	switch target {
	case "SUBSCRIBER":
		_ = p.pcSub.AddICECandidate(init)
	case "PUBLISHER":
		_ = p.pcPub.AddICECandidate(init)
	}
}

func (p *Peer) sendAck(uid string) {
	if uid == "" {
		return
	}

	p.wsMu.Lock()
	defer p.wsMu.Unlock()

	_ = p.ws.WriteJSON(map[string]interface{}{
		"uid": uid,
		"ack": map[string]interface{}{
			"status": map[string]interface{}{"code": "OK"},
		},
	})
}

func (p *Peer) registerAckWaiter(uid string) chan struct{} {
	ch := make(chan struct{})
	p.ackMu.Lock()
	p.ackWaiters[uid] = ch
	p.ackMu.Unlock()
	return ch
}

func (p *Peer) removeAckWaiter(uid string) {
	p.ackMu.Lock()
	delete(p.ackWaiters, uid)
	p.ackMu.Unlock()
}

func (p *Peer) waitForAck(uid string, ch <-chan struct{}, timeout time.Duration) bool {
	if uid == "" {
		return false
	}

	defer p.removeAckWaiter(uid)

	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	case <-p.closeCh:
		return false
	}
}

func (p *Peer) resolveAck(uid string) {
	if uid == "" {
		return
	}

	p.ackMu.Lock()
	ch := p.ackWaiters[uid]
	if ch != nil {
		delete(p.ackWaiters, uid)
		close(ch)
	}
	p.ackMu.Unlock()
}

func (p *Peer) sendPong(uid string) {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()

	_ = p.ws.WriteJSON(map[string]interface{}{
		"uid":  uid,
		"pong": map[string]interface{}{},
	})
}

func (p *Peer) startTelemetry(ctx context.Context, serverHello map[string]interface{}) { //nolint:cyclop
	cfg, ok := serverHello["telemetryConfiguration"].(map[string]interface{})
	if !ok {
		return
	}

	endpoint, _ := cfg["logEndpoint"].(string)
	if endpoint == "" {
		endpoint, _ = cfg["endpoint"].(string)
	}
	if endpoint == "" {
		endpoint, _ = cfg["url"].(string)
	}
	if endpoint == "" {
		logger.Verbosef("Telemetry endpoint missing")
		return
	}

	interval := defaultTelemetryInterval
	if raw, ok := cfg["sendingInterval"].(float64); ok && raw > 0 {
		interval = time.Duration(raw) * time.Millisecond
	}

	if !p.telemetryActive.CompareAndSwap(false, true) {
		return
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer p.telemetryActive.Store(false)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		p.sendTelemetry(ctx, endpoint, "join")
		for {
			select {
			case <-ticker.C:
				p.sendTelemetry(ctx, endpoint, "stats")
			case <-p.telemetryCh:
				p.sendTelemetry(ctx, endpoint, "leave")
				return
			case <-p.closeCh:
				p.sendTelemetry(ctx, endpoint, "leave")
				return
			}
		}
	}()
}

func (p *Peer) stopTelemetry() {
	if p.telemetryActive.Load() {
		select {
		case p.telemetryCh <- struct{}{}:
		default:
		}
	}
}

func (p *Peer) sendTelemetry(ctx context.Context, endpoint, event string) {
	body, err := json.Marshal(map[string]interface{}{
		"event":          event,
		"timestamp":      time.Now().UnixMilli(),
		"peerId":         p.conn.PeerID,
		"roomId":         p.conn.RoomID,
		"displayName":    p.name,
		"implementation": "olcrtc-go",
		"dataChannel": map[string]interface{}{
			"bufferedAmount": p.GetBufferedAmount(),
			"sendQueue":      len(p.sendQueue),
		},
	})
	if err != nil {
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		logger.Verbosef("Telemetry req error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:149.0) Gecko/20100101 Firefox/149.0")
	req.Header.Set("Origin", "https://telemost.yandex.ru")
	req.Header.Set("Referer", p.roomURL)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Client-Instance-Id", uuid.New().String())
	req.Header.Set("X-Telemost-Client-Version", "187.1.0")
	req.Header.Set("Idempotency-Key", uuid.New().String())

	client := protect.NewHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		logger.Verbosef("Telemetry send error: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
}

func (p *Peer) signalEnded(reason string) {
	log.Printf("Conference ended: %s", reason)
	p.closed.Store(true)
	p.stopTelemetry()
	if p.onEnded != nil {
		p.onEnded(reason)
	}
}

func isConferenceEndMessage(msg map[string]interface{}) bool {
	for _, key := range []string{"conferenceClosed", "conferenceEnded", "roomClosed", "roomEnded", "callEnded"} {
		if _, ok := msg[key]; ok {
			return true
		}
	}

	if raw, ok := msg["conference"].(map[string]interface{}); ok {
		if state, _ := raw["state"].(string); isEndedState(state) {
			return true
		}
	}

	if raw, ok := msg["conferenceState"].(map[string]interface{}); ok {
		if state, _ := raw["state"].(string); isEndedState(state) {
			return true
		}
	}

	return false
}

func isEndedState(state string) bool {
	switch strings.ToLower(state) {
	case "closed", "ended", "finished", "terminated":
		return true
	default:
		return false
	}
}

func (p *Peer) setupICEHandlers() {
	p.pcSub.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		p.wsMu.Lock()
		_ = p.ws.WriteJSON(map[string]interface{}{
			"uid": uuid.New().String(),
			"webrtcIceCandidate": map[string]interface{}{
				"candidate":     init.Candidate,
				"sdpMid":        init.SDPMid,
				"sdpMlineIndex": init.SDPMLineIndex,
				"target":        "SUBSCRIBER",
				"pcSeq":         1,
			},
		})
		p.wsMu.Unlock()
	})

	p.pcPub.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		p.wsMu.Lock()
		_ = p.ws.WriteJSON(map[string]interface{}{
			"uid": uuid.New().String(),
			"webrtcIceCandidate": map[string]interface{}{
				"candidate":     init.Candidate,
				"sdpMid":        init.SDPMid,
				"sdpMlineIndex": init.SDPMLineIndex,
				"target":        "PUBLISHER",
				"pcSeq":         1,
			},
		})
		p.wsMu.Unlock()
	})
}

func (p *Peer) sendLeave(uid string) bool {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()

	if p.ws == nil {
		return false
	}

	leave := map[string]interface{}{
		"uid":   uid,
		"leave": map[string]interface{}{},
	}

	if err := p.ws.WriteJSON(leave); err != nil {
		log.Printf("Failed to send leave: %v", err)
		return false
	}
	log.Println("Sent leave message")
	return true
}

// Close closes the peer connection and cleans up resources.
func (p *Peer) Close() error {
	log.Println("Closing peer...")
	alreadyClosing := p.closed.Swap(true)
	p.sendQueueClosed.Store(true)

	if !alreadyClosing {
		leaveUID := uuid.New().String()
		leaveAck := p.registerAckWaiter(leaveUID)
		if p.sendLeave(leaveUID) {
			_ = p.waitForAck(leaveUID, leaveAck, 1500*time.Millisecond)
		} else {
			p.removeAckWaiter(leaveUID)
		}
		p.stopTelemetry()
	}

	closeSignal(p.closeCh)

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		log.Println("Wait timeout")
	}

	if p.dc != nil {
		_ = p.dc.Close()
	}
	if p.pcPub != nil {
		_ = p.pcPub.Close()
	}
	if p.pcSub != nil {
		_ = p.pcSub.Close()
	}
	if p.ws != nil {
		p.wsMu.Lock()
		_ = p.ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second))
		_ = p.ws.Close()
		p.wsMu.Unlock()
	}

	return nil
}

func (p *Peer) keepAlive(keepAliveCh <-chan struct{}) {
	wsTicker := time.NewTicker(30 * time.Second)
	defer wsTicker.Stop()
	appTicker := time.NewTicker(5 * time.Second)
	defer appTicker.Stop()

	for {
		select {
		case <-wsTicker.C:
			if !p.sendWSPing() {
				return
			}
		case <-appTicker.C:
			if !p.sendAppPing() {
				return
			}
		case <-keepAliveCh:
			return
		case <-p.closeCh:
			return
		}
	}
}

func (p *Peer) sendWSPing() bool {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	if p.ws != nil {
		if err := p.ws.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
			log.Printf("WS Ping error: %v", err)
			p.queueReconnect()
			return false
		}
	}
	return true
}

func (p *Peer) sendAppPing() bool {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	if p.ws != nil {
		if err := p.ws.WriteJSON(map[string]interface{}{
			"uid":  uuid.New().String(),
			"ping": map[string]interface{}{},
		}); err != nil {
			log.Printf("App Ping error: %v", err)
			p.queueReconnect()
			return false
		}
	}
	return true
}

func (p *Peer) reconnect(ctx context.Context) error {
	p.reconnecting.Store(true)
	defer p.reconnecting.Store(false)

	p.sendLeave(uuid.New().String())
	time.Sleep(500 * time.Millisecond)
	p.stopSession()

	if p.dc != nil {
		_ = p.dc.Close()
	}
	if p.pcPub != nil {
		_ = p.pcPub.Close()
	}
	if p.pcSub != nil {
		_ = p.pcSub.Close()
	}
	if p.ws != nil {
		p.wsMu.Lock()
		_ = p.ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second))
		_ = p.ws.Close()
		p.wsMu.Unlock()
	}

	time.Sleep(3 * time.Second)
	conn, err := GetConnectionInfo(ctx, p.roomURL, p.name)
	if err != nil {
		return fmt.Errorf("reconnect get info: %w", err)
	}
	p.conn = conn

	if err := p.Connect(ctx); err != nil {
		return err
	}

	if p.onReconnect != nil {
		p.onReconnect(p.dc)
	}
	p.drainReconnectQueue()
	return nil
}

func (p *Peer) SetReconnectCallback(cb func(*webrtc.DataChannel)) { //nolint:revive
	p.onReconnect = cb
}

func (p *Peer) SetShouldReconnect(fn func() bool) { //nolint:revive
	p.shouldReconnect = fn
}

func (p *Peer) WatchConnection(ctx context.Context) { //nolint:revive,cyclop
	const maxReconnects = 10
	const reconnectWindow = 5 * time.Minute

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.closeCh:
			return
		case <-p.reconnectCh:
			if time.Since(p.lastReconnect) > reconnectWindow {
				p.reconnectCount = 0
			}
			p.reconnectCount++
			p.lastReconnect = time.Now()

			if p.reconnectCount > maxReconnects {
				log.Printf("Max reconnects reached (%d)", maxReconnects)
				p.signalEnded("reconnect limit reached")
				return
			}

			backoff := time.Duration(p.reconnectCount) * 2 * time.Second
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}

			for {
				if err := p.reconnect(ctx); err != nil {
					log.Printf("Reconnect failed: %v", err)
					select {
					case <-ctx.Done():
						return
					case <-p.closeCh:
						return
					case <-time.After(backoff):
						continue
					}
				}
				break
			}
		}
	}
}

func (p *Peer) processSendQueue(workerID int, sessionCloseCh <-chan struct{}) {
	for {
		select {
		case <-sessionCloseCh:
			return
		case <-p.closeCh:
			return
		case data := <-p.sendQueue:
			if len(data) > p.trafficShape.MaxMessageSize {
				log.Printf("[WORKER-%d] Refusing oversized message size=%d limit=%d",
					workerID, len(data), p.trafficShape.MaxMessageSize)
				continue
			}

			waited, err := p.waitBufferedAmount(workerID, sessionCloseCh)
			if err != nil {
				return
			}
			if waited > 0 {
				logger.Verbosef("[WORKER-%d] Drained after %v", workerID, waited)
			}

			if err := p.dc.Send(data); err != nil {
				log.Printf("[WORKER-%d] Send error: %v", workerID, err)
				p.queueReconnect()
				return
			}

			if p.trafficShape.MinDelay > 0 {
				time.Sleep(p.calculateDelay())
			}
		}
	}
}

func (p *Peer) waitBufferedAmount(workerID int, sessionCloseCh <-chan struct{}) (time.Duration, error) {
	start := time.Now()
	for p.dc.BufferedAmount() > 512*1024 {
		select {
		case <-sessionCloseCh:
			return 0, ErrSessionClosed
		case <-p.closeCh:
			return 0, ErrPeerClosed
		case <-time.After(10 * time.Millisecond):
			if time.Since(start) > 5*time.Second {
				log.Printf("[WORKER-%d] Buffer wait timeout", workerID)
				return time.Since(start), nil
			}
		}
	}
	return time.Since(start), nil
}

func (p *Peer) calculateDelay() time.Duration {
	minDelay := p.trafficShape.MinDelay
	maxDelay := p.trafficShape.MaxDelay
	if maxDelay <= minDelay {
		return minDelay
	}
	//nolint:gosec
	return minDelay + time.Duration(rand.Int64N(int64(maxDelay-minDelay)))
}

func (p *Peer) monitorQueue(sessionCloseCh <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sessionCloseCh:
			return
		case <-p.closeCh:
			return
		case <-ticker.C:
			queueLen := len(p.sendQueue)
			buffered := p.dc.BufferedAmount()
			if queueLen > 100 || buffered > 1024*1024 {
				log.Printf("[MONITOR] queue=%d, buffered=%d MB",
					queueLen, buffered/(1024*1024))
			}
		}
	}
}

func (p *Peer) CanSend() bool { //nolint:revive
	if p.dc == nil || p.dc.ReadyState() != webrtc.DataChannelStateOpen {
		return false
	}
	return len(p.sendQueue) < 4000
}
