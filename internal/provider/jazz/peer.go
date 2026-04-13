package jazz

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
	"github.com/openlibrecommunity/olcrtc/internal/provider"
	"github.com/pion/webrtc/v4"
)

const (
	maxDataChannelMessageSize = 12288
	sendDelay                 = 2 * time.Millisecond
)

type Peer struct {
	name            string
	roomInfo        *RoomInfo
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
	closed          atomic.Bool
	reconnecting    atomic.Bool
	sendQueue       chan []byte
	sendQueueClosed atomic.Bool
	onEnded         func(string)
	sessionCloseCh  chan struct{}
	wg              sync.WaitGroup
	groupID         string
}

// NewPeer creates a new Jazz provider peer.
func NewPeer(ctx context.Context, roomID, name string, onData func([]byte)) (*Peer, error) {
	var roomInfo *RoomInfo
	var err error

	if roomID == "" || roomID == "any" || roomID == "dummy" {
		roomInfo, err = createRoom(ctx)
		if err != nil {
			return nil, fmt.Errorf("create room: %w", err)
		}
		log.Printf("Jazz room created: %s:%s", roomInfo.RoomID, roomInfo.Password)
		log.Printf("To connect client use: -id \"%s:%s\"", roomInfo.RoomID, roomInfo.Password)
	} else {
		var password string
		parts := strings.Split(roomID, ":")
		if len(parts) == 2 {
			roomID = parts[0]
			password = parts[1]
		}

		roomInfo, err = joinRoom(ctx, roomID, password)
		if err != nil {
			return nil, fmt.Errorf("join room: %w", err)
		}
		log.Printf("Jazz joining room: %s", roomInfo.RoomID)
	}

	return &Peer{
		name:           name,
		roomInfo:       roomInfo,
		onData:         onData,
		reconnectCh:    make(chan struct{}, 1),
		closeCh:        make(chan struct{}),
		sessionCloseCh: make(chan struct{}),
		sendQueue:      make(chan []byte, 5000),
	}, nil
}

func (p *Peer) Connect(ctx context.Context) error {
	p.closed.Store(false)

	config := webrtc.Configuration{
		ICEServers:   []webrtc.ICEServer{},
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
		BundlePolicy: webrtc.BundlePolicyMaxBundle,
	}

	settingEngine := webrtc.SettingEngine{}
	if protect.Protector != nil {
		settingEngine.SetICEProxyDialer(protect.NewProxyDialer())
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine))

	var err error
	p.pcSub, err = api.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("create subscriber pc: %w", err)
	}

	p.pcPub, err = api.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("create publisher pc: %w", err)
	}

	p.dc, err = p.pcPub.CreateDataChannel("_reliable", &webrtc.DataChannelInit{
		Ordered: func() *bool { v := true; return &v }(),
	})
	if err != nil {
		return fmt.Errorf("create datachannel: %w", err)
	}

	dcReady := make(chan struct{})
	p.setupDataChannelHandlers(dcReady)

	if err := p.dialWebSocket(); err != nil {
		return err
	}

	if err := p.sendJoin(); err != nil {
		return err
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.handleSignaling(ctx)
	}()

	select {
	case <-dcReady:
		return nil
	case <-time.After(30 * time.Second):
		return provider.ErrDataChannelTimeout
	case <-ctx.Done():
		return fmt.Errorf("connect cancelled: %w", ctx.Err())
	}
}

func (p *Peer) dialWebSocket() error {
	wsDialer := websocket.Dialer{
		NetDialContext:   protect.DialContext,
		HandshakeTimeout: 15 * time.Second,
	}

	ws, resp, err := wsDialer.Dial(p.roomInfo.ConnectorURL, nil)
	if err != nil {
		return fmt.Errorf("dial websocket: %w", err)
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

func (p *Peer) sendJoin() error {
	joinMsg := map[string]any{
		"roomId":    p.roomInfo.RoomID,
		"event":     "join",
		"requestId": uuid.New().String(),
		"payload": map[string]any{
			"password":        p.roomInfo.Password,
			"participantName": p.name,
			"supportedFeatures": map[string]any{
				"attachedRooms": true,
				"sessionGroups": true,
				"transcription": true,
			},
			"isSilent": false,
		},
	}

	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	return p.ws.WriteJSON(joinMsg)
}

func (p *Peer) setupDataChannelHandlers(dcReady chan struct{}) {
	p.dc.OnOpen(func() {
		logger.Verbosef("[Jazz] Publisher DC opened: %s", p.dc.Label())
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.processSendQueue()
		}()
		close(dcReady)
	})

	p.dc.OnClose(func() {
		logger.Verbosef("[Jazz] Publisher DC closed")
		if !p.closed.Load() {
			p.queueReconnect()
		}
	})

	p.dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		logger.Verbosef("[Jazz] Received %d bytes on publisher DC (raw)", len(msg.Data))

		payload, ok := DecodeDataPacket(msg.Data)
		if !ok {
			logger.Debugf("[Jazz] Failed to decode DataPacket, trying raw")
			if p.onData != nil && len(msg.Data) > 0 {
				p.onData(msg.Data)
			}
			return
		}

		logger.Verbosef("[Jazz] Decoded DataPacket: %d bytes payload", len(payload))
		if p.onData != nil && len(payload) > 0 {
			p.onData(payload)
		}
	})

	p.pcSub.OnDataChannel(func(dc *webrtc.DataChannel) {
		logger.Verbosef("[Jazz] Received subscriber DataChannel: %s", dc.Label())
		if dc.Label() != "_reliable" {
			return
		}

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			logger.Verbosef("[Jazz] Received %d bytes on subscriber DC (_reliable, raw)", len(msg.Data))

			payload, ok := DecodeDataPacket(msg.Data)
			if !ok {
				logger.Debugf("[Jazz] Failed to decode DataPacket from subscriber, trying raw")
				if p.onData != nil && len(msg.Data) > 0 {
					p.onData(msg.Data)
				}
				return
			}

			logger.Verbosef("[Jazz] Decoded DataPacket from subscriber: %d bytes payload", len(payload))
			if p.onData != nil && len(payload) > 0 {
				p.onData(payload)
			}
		})
	})
}

func (p *Peer) handleSignaling(ctx context.Context) {
	for {
		var msg map[string]any
		if err := p.ws.ReadJSON(&msg); err != nil {
			logger.Debugf("ws read error: %v", err)
			if !p.closed.Load() {
				p.queueReconnect()
			}
			return
		}

		p.updateWSDeadline()

		event, _ := msg["event"].(string)
		payload, _ := msg["payload"].(map[string]any)

		switch event {
		case "join-response":
			p.handleJoinResponse(payload)
		case "media-out":
			p.handleMediaOut(ctx, payload)
		}
	}
}

func (p *Peer) handleJoinResponse(payload map[string]any) {
	group, _ := payload["participantGroup"].(map[string]any)
	p.groupID, _ = group["groupId"].(string)
	logger.Verbosef("Jazz peer joined: groupId=%s", p.groupID)
}

func (p *Peer) handleMediaOut(ctx context.Context, payload map[string]any) {
	method, _ := payload["method"].(string)

	switch method {
	case "rtc:config":
		p.handleRTCConfig(payload)
	case "rtc:join":
		logger.Verbosef("Jazz rtc:join received")
	case "rtc:offer":
		p.handleSubscriberOffer(ctx, payload)
	case "rtc:answer":
		p.handlePublisherAnswer(payload)
	case "rtc:ice":
		p.handleICE(payload)
	}
}

func (p *Peer) handleRTCConfig(payload map[string]any) {
	config, _ := payload["configuration"].(map[string]any)
	servers, _ := config["iceServers"].([]any)

	var iceServers []webrtc.ICEServer
	for _, s := range servers {
		server, _ := s.(map[string]any)
		urls, _ := server["urls"].([]any)
		username, _ := server["username"].(string)
		credential, _ := server["credential"].(string)

		var urlStrs []string
		for _, u := range urls {
			if urlStr, ok := u.(string); ok && urlStr != "" {
				urlStrs = append(urlStrs, urlStr)
			}
		}

		if len(urlStrs) > 0 {
			iceServers = append(iceServers, webrtc.ICEServer{
				URLs:       urlStrs,
				Username:   username,
				Credential: credential,
			})
		}
	}

	if len(iceServers) > 0 {
		newConfig := webrtc.Configuration{
			ICEServers:   iceServers,
			SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
			BundlePolicy: webrtc.BundlePolicyMaxBundle,
		}
		_ = p.pcSub.SetConfiguration(newConfig)
		_ = p.pcPub.SetConfiguration(newConfig)
	}
}

func (p *Peer) handleSubscriberOffer(ctx context.Context, payload map[string]any) {
	desc, _ := payload["description"].(map[string]any)
	sdp, _ := desc["sdp"].(string)

	if err := p.pcSub.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}); err != nil {
		logger.Debugf("set remote desc error: %v", err)
		return
	}

	answer, err := p.pcSub.CreateAnswer(nil)
	if err != nil {
		logger.Debugf("create answer error: %v", err)
		return
	}

	if err := p.pcSub.SetLocalDescription(answer); err != nil {
		logger.Debugf("set local desc error: %v", err)
		return
	}

	p.wsMu.Lock()
	_ = p.ws.WriteJSON(map[string]any{
		"roomId":    p.roomInfo.RoomID,
		"event":     "media-in",
		"groupId":   p.groupID,
		"requestId": uuid.New().String(),
		"payload": map[string]any{
			"method": "rtc:answer",
			"description": map[string]any{
				"type": "answer",
				"sdp":  answer.SDP,
			},
		},
	})
	p.wsMu.Unlock()

	time.Sleep(300 * time.Millisecond)
	p.sendPublisherOffer()
}

func (p *Peer) sendPublisherOffer() {
	offer, err := p.pcPub.CreateOffer(nil)
	if err != nil {
		logger.Debugf("create pub offer error: %v", err)
		return
	}

	if err := p.pcPub.SetLocalDescription(offer); err != nil {
		logger.Debugf("set local pub desc error: %v", err)
		return
	}

	p.wsMu.Lock()
	_ = p.ws.WriteJSON(map[string]any{
		"roomId":    p.roomInfo.RoomID,
		"event":     "media-in",
		"groupId":   p.groupID,
		"requestId": uuid.New().String(),
		"payload": map[string]any{
			"method": "rtc:offer",
			"description": map[string]any{
				"type": "offer",
				"sdp":  offer.SDP,
			},
		},
	})
	p.wsMu.Unlock()
}

func (p *Peer) handlePublisherAnswer(payload map[string]any) {
	desc, _ := payload["description"].(map[string]any)
	sdp, _ := desc["sdp"].(string)

	if err := p.pcPub.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	}); err != nil {
		logger.Debugf("set remote pub desc error: %v", err)
	}
}

func (p *Peer) handleICE(payload map[string]any) {
	candidates, _ := payload["rtcIceCandidates"].([]any)

	for _, c := range candidates {
		cand, _ := c.(map[string]any)
		candStr, _ := cand["candidate"].(string)
		target, _ := cand["target"].(string)
		sdpMid, _ := cand["sdpMid"].(string)
		sdpMLineIndex, _ := cand["sdpMLineIndex"].(float64)

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
}

func (p *Peer) updateWSDeadline() {
	p.wsMu.Lock()
	if p.ws != nil {
		_ = p.ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	}
	p.wsMu.Unlock()
}

func (p *Peer) Send(data []byte) error {
	if p.dc == nil || p.dc.ReadyState() != webrtc.DataChannelStateOpen {
		return provider.ErrDataChannelNotReady
	}

	if p.sendQueueClosed.Load() {
		return provider.ErrSendQueueClosed
	}

	select {
	case p.sendQueue <- data:
		return nil
	case <-time.After(50 * time.Millisecond):
		return provider.ErrSendQueueTimeout
	}
}

func (p *Peer) processSendQueue() {
	for {
		select {
		case <-p.sessionCloseCh:
			return
		case <-p.closeCh:
			return
		case data := <-p.sendQueue:
			if len(data) > maxDataChannelMessageSize {
				logger.Debugf("[Jazz] Message too large: %d bytes (max %d)", len(data), maxDataChannelMessageSize)
				continue
			}

			encoded := EncodeDataPacket(data)
			logger.Verbosef("[Jazz] Sending %d bytes (encoded to %d bytes)", len(data), len(encoded))

			if err := p.dc.Send(encoded); err != nil {
				logger.Debugf("send error: %v", err)
				p.queueReconnect()
				return
			}
			time.Sleep(sendDelay)
		}
	}
}

func (p *Peer) Close() error {
	p.closed.Store(true)
	p.sendQueueClosed.Store(true)

	close(p.closeCh)

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
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

func (p *Peer) SetReconnectCallback(cb func(*webrtc.DataChannel)) {
	p.onReconnect = cb
}

func (p *Peer) SetShouldReconnect(fn func() bool) {
	p.shouldReconnect = fn
}

func (p *Peer) SetEndedCallback(cb func(string)) {
	p.onEnded = cb
}

func (p *Peer) WatchConnection(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.closeCh:
			return
		case <-p.reconnectCh:
		}
	}
}

func (p *Peer) CanSend() bool {
	if p.dc == nil || p.dc.ReadyState() != webrtc.DataChannelStateOpen {
		return false
	}
	return len(p.sendQueue) < 4000
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

func (p *Peer) queueReconnect() {
	if p.closed.Load() || p.reconnecting.Load() {
		return
	}
	if p.shouldReconnect != nil && !p.shouldReconnect() {
		return
	}
	select {
	case p.reconnectCh <- struct{}{}:
	default:
	}
}
