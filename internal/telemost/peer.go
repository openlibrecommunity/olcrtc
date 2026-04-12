package telemost

import (
	"bytes"
	"context"
	"encoding/json"
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
	ErrDataChannelTimeout  = fmt.Errorf("datachannel timeout")
	ErrDataChannelNotReady = fmt.Errorf("datachannel not ready")
	ErrSendQueueClosed     = fmt.Errorf("send queue closed")
	ErrSendQueueTimeout    = fmt.Errorf("send queue timeout")
)

type TrafficShape struct {
	MaxMessageSize int
	MinDelay       time.Duration
	MaxDelay       time.Duration
}

type Peer struct {
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
	reconnectMu     sync.Mutex
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

func (p *Peer) GetSendQueue() chan []byte {
	return p.sendQueue
}

func (p *Peer) GetBufferedAmount() uint64 {
	if p.dc != nil {
		return p.dc.BufferedAmount()
	}
	return 0
}

func (p *Peer) SetEndedCallback(cb func(string)) {
	p.onEnded = cb
}

func (p *Peer) SetTrafficShape(shape TrafficShape) {
	if shape.MaxMessageSize <= 0 {
		shape.MaxMessageSize = realDataChannelMessageLimit
	}
	if shape.MaxDelay < shape.MinDelay {
		shape.MaxDelay = shape.MinDelay
	}
	p.trafficShape = shape
}

func NewPeer(roomURL, name string, onData func([]byte)) (*Peer, error) {
	conn, err := GetConnectionInfo(context.Background(), roomURL, name)
	if err != nil {
		return nil, err
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

func (p *Peer) Connect(ctx context.Context) error {
	p.closed.Store(false)

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.rtc.yandex.net:3478"}},
		},
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	}

	settingEngine := webrtc.SettingEngine{}
	if protect.Protector != nil {
		settingEngine.SetICEProxyDialer(protect.NewProxyDialer())
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine))

	var err error
	p.pcSub, err = api.NewPeerConnection(config)
	if err != nil {
		return err
	}

	p.pcSub.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Subscriber PeerConnection state: %s", state.String())
		if !p.closed.Load() && (state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected) {
			p.queueReconnect()
		}
	})

	p.pcPub, err = api.NewPeerConnection(config)
	if err != nil {
		return err
	}

	p.pcPub.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Publisher PeerConnection state: %s", state.String())
		if !p.closed.Load() && (state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected) {
			p.queueReconnect()
		}
	})

	p.dc, err = p.pcPub.CreateDataChannel("olcrtc", nil)
	if err != nil {
		return err
	}

	dcReady := make(chan struct{})
	keepAliveCh, sessionCloseCh := p.resetSession()
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

	p.dc.OnClose(func() {
		log.Println("DataChannel closed")
		if p.onReconnect != nil {
			log.Println("Calling reconnect callback for cleanup")
			p.onReconnect(nil)
		}
		if !p.closed.Load() {
			p.queueReconnect()
		}
	})

	p.dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if p.onData != nil && len(msg.Data) > 0 {
			p.onData(msg.Data)
		}
	})

	p.pcSub.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("Received datachannel: %s", dc.Label())
		dc.OnClose(func() {
			log.Println("Received DataChannel closed - triggering reconnect")
			if !p.closed.Load() {
				p.queueReconnect()
			}
		})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if p.onData != nil && len(msg.Data) > 0 {
				p.onData(msg.Data)
			}
		})
	})

	wsDialer := websocket.Dialer{
		NetDialContext:   protect.DialContext,
		HandshakeTimeout: 15 * time.Second,
	}
	ws, resp, err := wsDialer.Dial(p.conn.ClientConfig.MediaServerURL, nil)
	if err != nil {
		return fmt.Errorf("failed to dial websocket: %w", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	p.ws = ws

	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	ws.SetReadDeadline(time.Now().Add(60 * time.Second))

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.keepAlive(keepAliveCh)
	}()

	if err := p.sendHello(); err != nil {
		return err
	}

	p.setupICEHandlers()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.handleSignaling()
	}()

	select {
	case <-dcReady:
		return nil
	case <-time.After(15 * time.Second):
		return ErrDataChannelTimeout
	case <-ctx.Done():
		return fmt.Errorf("connect context cancelled: %w", ctx.Err())
	}
}

func (p *Peer) Send(data []byte) error {
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
		log.Printf("[SEND_QUEUE] Timeout! queue_len=%d, dropping packet size=%d", queueLen, len(data))
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
	return p.ws.WriteJSON(hello)
}

func (p *Peer) handleSignaling() {
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

		p.wsMu.Lock()
		if p.ws != nil {
			p.ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		}
		p.wsMu.Unlock()

		uid, _ := msg["uid"].(string)

		if _, ok := msg["ack"]; ok {
			p.resolveAck(uid)
		}

		if serverHello, ok := msg["serverHello"].(map[string]interface{}); ok {
			p.startTelemetry(serverHello)
			p.sendAck(uid)
		}

		if _, ok := msg["updateDescription"]; ok {
			p.sendAck(uid)
		}

		if _, ok := msg["vadActivity"]; ok {
			p.sendAck(uid)
		}

		if isConferenceEndMessage(msg) {
			p.signalEnded("conference ended")
			return
		}

		if _, ok := msg["ping"]; ok {
			p.sendPong(uid)
			continue
		}

		if _, ok := msg["pong"]; ok {
			p.sendAck(uid)
			continue
		}

		if offer, ok := msg["subscriberSdpOffer"].(map[string]interface{}); ok && !pubSent {
			sdp, _ := offer["sdp"].(string)
			pcSeq, _ := offer["pcSeq"].(float64)

			if err := p.pcSub.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  sdp,
			}); err != nil {
				log.Printf("SetRemoteDescription error: %v", err)
				continue
			}

			answer, err := p.pcSub.CreateAnswer(nil)
			if err != nil {
				log.Printf("CreateAnswer error: %v", err)
				continue
			}

			if err := p.pcSub.SetLocalDescription(answer); err != nil {
				log.Printf("SetLocalDescription error: %v", err)
				continue
			}

			p.wsMu.Lock()
			p.ws.WriteJSON(map[string]interface{}{
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
				log.Printf("CreateOffer error: %v", err)
				continue
			}

			if err := p.pcPub.SetLocalDescription(pubOffer); err != nil {
				log.Printf("SetLocalDescription error: %v", err)
				continue
			}

			p.wsMu.Lock()
			p.ws.WriteJSON(map[string]interface{}{
				"uid": uuid.New().String(),
				"publisherSdpOffer": map[string]interface{}{
					"pcSeq": 1,
					"sdp":   pubOffer.SDP,
				},
			})
			p.wsMu.Unlock()

			pubSent = true
		}

		if answer, ok := msg["publisherSdpAnswer"].(map[string]interface{}); ok {
			sdp, _ := answer["sdp"].(string)

			if err := p.pcPub.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer,
				SDP:  sdp,
			}); err != nil {
				log.Printf("SetRemoteDescription error: %v", err)
			}

			p.sendAck(uid)
		}

		if cand, ok := msg["webrtcIceCandidate"].(map[string]interface{}); ok {
			p.handleICE(cand)
		}
	}
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

	if target == "SUBSCRIBER" {
		_ = p.pcSub.AddICECandidate(init)
	} else if target == "PUBLISHER" {
		_ = p.pcPub.AddICECandidate(init)
	}
}

func (p *Peer) sendAck(uid string) {
	if uid == "" {
		return
	}

	p.wsMu.Lock()
	defer p.wsMu.Unlock()

	p.ws.WriteJSON(map[string]interface{}{
		"uid": uid,
		"ack": map[string]interface{}{
			"status": map[string]interface{}{
				"code": "OK",
			},
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

	defer func() {
		p.removeAckWaiter(uid)
	}()

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

	p.ws.WriteJSON(map[string]interface{}{
		"uid":  uid,
		"pong": map[string]interface{}{},
	})
}

func (p *Peer) startTelemetry(serverHello map[string]interface{}) {
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
		logger.Verbose("Telemetry configuration has no endpoint; skipping XHR simulation")
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

		p.sendTelemetry(endpoint, "join")
		for {
			select {
			case <-ticker.C:
				p.sendTelemetry(endpoint, "stats")
			case <-p.telemetryCh:
				p.sendTelemetry(endpoint, "leave")
				return
			case <-p.closeCh:
				p.sendTelemetry(endpoint, "leave")
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

func (p *Peer) sendTelemetry(endpoint, event string) {
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

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		logger.Verbose("Telemetry request skipped: %v", err)
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
		logger.Verbose("Telemetry send failed: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		logger.Verbose("Telemetry endpoint returned %s", resp.Status)
	}
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
		p.ws.WriteJSON(map[string]interface{}{
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
		p.ws.WriteJSON(map[string]interface{}{
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
		log.Println("WebSocket already closed, cannot send leave")
		return false
	}

	leave := map[string]interface{}{
		"uid":   uid,
		"leave": map[string]interface{}{},
	}

	if err := p.ws.WriteJSON(leave); err != nil {
		log.Printf("Failed to send leave: %v", err)
		return false
	} else {
		log.Println("Sent leave message to server")
	}
	return true
}

func (p *Peer) Close() error {
	log.Println("Closing peer connection...")

	alreadyClosing := p.closed.Swap(true)
	p.sendQueueClosed.Store(true)

	if !alreadyClosing {
		log.Println("Sending leave message...")
		leaveUID := uuid.New().String()
		leaveAck := p.registerAckWaiter(leaveUID)
		if p.sendLeave(leaveUID) {
			if p.waitForAck(leaveUID, leaveAck, 1500*time.Millisecond) {
				log.Println("Leave acknowledged")
			} else {
				log.Println("Leave ack timeout")
			}
		} else {
			p.removeAckWaiter(leaveUID)
		}

		p.stopTelemetry()
	}

	log.Println("Closing channels...")
	if p.closeCh != nil {
		select {
		case <-p.closeCh:
		default:
			close(p.closeCh)
		}
	}

	log.Println("Waiting for goroutines...")
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("All goroutines finished")
	case <-time.After(2 * time.Second):
		log.Println("Goroutine wait timeout")
	}

	if p.dc != nil {
		log.Println("Closing DataChannel...")
		_ = p.dc.Close()
	}

	if p.pcPub != nil {
		log.Println("Closing Publisher PeerConnection...")
		_ = p.pcPub.Close()
	}

	if p.pcSub != nil {
		log.Println("Closing Subscriber PeerConnection...")
		_ = p.pcSub.Close()
	}

	if p.ws != nil {
		log.Println("Closing WebSocket...")
		p.wsMu.Lock()
		_ = p.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = p.ws.Close()
		p.wsMu.Unlock()
	}

	log.Println("Peer closed")
	return nil
}

func (p *Peer) keepAlive(keepAliveCh <-chan struct{}) {
	wsPingTicker := time.NewTicker(30 * time.Second)
	defer wsPingTicker.Stop()

	appPingTicker := time.NewTicker(5 * time.Second)
	defer appPingTicker.Stop()

	for {
		select {
		case <-wsPingTicker.C:
			p.wsMu.Lock()
			if p.ws != nil {
				if err := p.ws.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
					log.Printf("WS Ping error: %v", err)
					p.wsMu.Unlock()
					p.queueReconnect()
					return
				}
			}
			p.wsMu.Unlock()
		case <-appPingTicker.C:
			p.wsMu.Lock()
			if p.ws != nil {
				if err := p.ws.WriteJSON(map[string]interface{}{
					"uid":  uuid.New().String(),
					"ping": map[string]interface{}{},
				}); err != nil {
					log.Printf("App Ping error: %v", err)
					p.wsMu.Unlock()
					p.queueReconnect()
					return
				}
			}
			p.wsMu.Unlock()
		case <-keepAliveCh:
			return
		case <-p.closeCh:
			return
		}
	}
}

func (p *Peer) reconnect(ctx context.Context) error {
	log.Println("Reconnecting...")
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
		_ = p.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = p.ws.Close()
		p.wsMu.Unlock()
	}

	time.Sleep(3 * time.Second)

	conn, err := GetConnectionInfo(ctx, p.roomURL, p.name)
	if err != nil {
		return fmt.Errorf("failed to get connection info: %w", err)
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

func (p *Peer) SetReconnectCallback(cb func(*webrtc.DataChannel)) {
	p.onReconnect = cb
}

func (p *Peer) SetShouldReconnect(fn func() bool) {
	p.shouldReconnect = fn
}

func (p *Peer) WatchConnection(ctx context.Context) {
	const maxReconnects = 10
	const reconnectWindow = 5 * time.Minute

	for {
		select {
		case <-p.reconnectCh:
			p.reconnectMu.Lock()
			now := time.Now()
			if now.Sub(p.lastReconnect) > reconnectWindow {
				p.reconnectCount = 0
			}

			if p.reconnectCount >= maxReconnects {
				log.Printf("Max reconnect attempts (%d) reached, stopping", maxReconnects)
				p.reconnectMu.Unlock()
				return
			}

			p.reconnectCount++
			p.lastReconnect = now
			p.reconnectMu.Unlock()

			backoff := time.Duration(p.reconnectCount) * 2 * time.Second
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}

			for {
				if err := p.reconnect(ctx); err != nil {
					log.Printf("Reconnect failed: %v, retrying in %v...", err, backoff)
					time.Sleep(backoff)
					continue
				}
				p.reconnectMu.Lock()
				p.reconnectCount = 0
				p.reconnectMu.Unlock()
				log.Println("Reconnected successfully")
				break
			}
		case <-p.closeCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (p *Peer) processSendQueue(workerID int, sessionCloseCh <-chan struct{}) {

	for {
		select {
		case data, ok := <-p.sendQueue:
			if !ok {
				return
			}
			if p.dc == nil || p.dc.ReadyState() != webrtc.DataChannelStateOpen {
				continue
			}
			if p.trafficShape.MaxMessageSize > 0 && len(data) > p.trafficShape.MaxMessageSize {
				log.Printf("[WORKER-%d] Refusing oversized DataChannel message size=%d limit=%d", workerID, len(data), p.trafficShape.MaxMessageSize)
				continue
			}
			if delay := p.nextSendDelay(); delay > 0 {
				time.Sleep(delay)
			}

			// Wait until SCTP buffer drains. Dropping here would corrupt the
			// carried TCP streams (the mux is a reliable transport); large
			// downloads like Instagram/Twitter assets would hang forever
			// waiting for the missing bytes. Backpressure already propagates
			// upstream via CanSend() / the sendQueue length.
			// Threshold is high (4MB) because a tight limit serialises sends:
			// workers would pause on every frame, turning throughput into
			// one chunk per 10ms drain cycle (~400KB/s).
			waitStart := time.Now()
			for p.dc.BufferedAmount() > 4*1024*1024 {
				if p.dc.ReadyState() != webrtc.DataChannelStateOpen {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if waited := time.Since(waitStart); waited > 500*time.Millisecond {
				logger.Verbose("[WORKER-%d] Buffer drained after %v", workerID, waited)
			}

			if p.dc == nil || p.dc.ReadyState() != webrtc.DataChannelStateOpen {
				continue
			}

			sendStart := time.Now()
			if err := p.dc.Send(data); err != nil {
				log.Printf("[WORKER-%d] Send error: %v", workerID, err)
			} else {
				elapsed := time.Since(sendStart)
				if elapsed > 50*time.Millisecond {
					log.Printf("[WORKER-%d] Sent %d bytes in %v (buffered: %d)",
						workerID, len(data), elapsed, p.dc.BufferedAmount())
				} else {
					logger.Verbose("[WORKER-%d] Sent %d bytes (buffered: %d)",
						workerID, len(data), p.dc.BufferedAmount())
				}
			}

		case <-sessionCloseCh:
			return
		case <-p.closeCh:
			return
		}
	}
}

func (p *Peer) monitorQueue(sessionCloseCh <-chan struct{}) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			queueLen := len(p.sendQueue)
			buffered := uint64(0)
			if p.dc != nil {
				buffered = p.dc.BufferedAmount()
			}
			if queueLen > 800 || buffered > 3*1024*1024 {
				log.Printf("[QUEUE_MONITOR] queue_len=%d dc_buffered=%d MB", queueLen, buffered/(1024*1024))
			}
		case <-sessionCloseCh:
			return
		case <-p.closeCh:
			return
		}
	}
}

func (p *Peer) CanSend() bool {
	queueLen := len(p.sendQueue)
	buffered := uint64(0)
	if p.dc != nil {
		buffered = p.dc.BufferedAmount()
	}
	return queueLen < 1000 && buffered < 3*1024*1024
}

func (p *Peer) nextSendDelay() time.Duration {
	minDelay := p.trafficShape.MinDelay
	maxDelay := p.trafficShape.MaxDelay
	if maxDelay <= 0 {
		return 0
	}
	if maxDelay <= minDelay {
		return maxDelay
	}
	return minDelay + time.Duration(rand.Int64N(int64(maxDelay-minDelay)))
}
