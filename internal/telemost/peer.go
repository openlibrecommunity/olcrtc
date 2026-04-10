package telemost

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
	"github.com/pion/webrtc/v4"
)

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
	reconnectCh     chan struct{}
	closeCh         chan struct{}
	keepAliveCh     chan struct{}
	lastReconnect   time.Time
	reconnectCount  int
	reconnectMu     sync.Mutex
	sendQueue       chan []byte
	sendQueueClosed atomic.Bool
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

func NewPeer(roomURL, name string, onData func([]byte)) (*Peer, error) {
	conn, err := GetConnectionInfo(roomURL, name)
	if err != nil {
		return nil, err
	}

	return &Peer{
		roomURL:     roomURL,
		name:        name,
		conn:        conn,
		onData:      onData,
		reconnectCh: make(chan struct{}, 1),
		closeCh:     make(chan struct{}),
		keepAliveCh: make(chan struct{}),
		sendQueue:   make(chan []byte, 5000),
	}, nil
}

func (p *Peer) Connect(ctx context.Context) error {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.rtc.yandex.net:3478"}},
		},
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	}

	settingEngine := webrtc.SettingEngine{}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine))

	var err error
	p.pcSub, err = api.NewPeerConnection(config)
	if err != nil {
		return err
	}
	
	p.pcSub.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Subscriber PeerConnection state: %s", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected {
			select {
			case p.reconnectCh <- struct{}{}:
			default:
			}
		}
	})

	p.pcPub, err = api.NewPeerConnection(config)
	if err != nil {
		return err
	}
	
	p.pcPub.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Publisher PeerConnection state: %s", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected {
			select {
			case p.reconnectCh <- struct{}{}:
			default:
			}
		}
	})

	p.dc, err = p.pcPub.CreateDataChannel("olcrtc", nil)
	if err != nil {
		return err
	}

	dcReady := make(chan struct{})
	p.dc.OnOpen(func() {
		log.Println("DataChannel opened")
		
		numWorkers := 4
		for i := 0; i < numWorkers; i++ {
			p.wg.Add(1)
			go func(workerID int) {
				defer p.wg.Done()
				p.processSendQueue(workerID)
			}(i)
		}
		
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.monitorQueue()
		}()
		
		close(dcReady)
	})
	
	p.dc.OnClose(func() {
		log.Println("DataChannel closed")
		if p.onReconnect != nil {
			log.Println("Calling reconnect callback for cleanup")
			p.onReconnect(nil)
		}
		select {
		case p.reconnectCh <- struct{}{}:
		default:
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
			select {
			case p.reconnectCh <- struct{}{}:
			default:
			}
		})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if p.onData != nil && len(msg.Data) > 0 {
				p.onData(msg.Data)
			}
		})
	})

	ws, _, err := websocket.DefaultDialer.Dial(p.conn.ClientConfig.MediaServerURL, nil)
	if err != nil {
		return err
	}
	p.ws = ws

	ws.SetPongHandler(func(string) error {
		if err := ws.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
			log.Printf("Error setting read deadline: %v", err)
		}
		return nil
	})
	
	if err := ws.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
		log.Printf("Error setting read deadline: %v", err)
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.keepAlive()
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
		return fmt.Errorf("datachannel timeout")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Peer) Send(data []byte) error {
	if p.dc == nil || p.dc.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("datachannel not ready")
	}
	
	if p.sendQueueClosed.Load() {
		return fmt.Errorf("send queue closed")
	}
	
	select {
	case p.sendQueue <- data:
		return nil
	case <-time.After(50 * time.Millisecond):
		queueLen := len(p.sendQueue)
		log.Printf("[SEND_QUEUE] Timeout! queue_len=%d, dropping packet size=%d", queueLen, len(data))
		return fmt.Errorf("send queue timeout")
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
			"sendAudio":         false,
			"sendVideo":         false,
			"sendSharing":       false,
			"participantId":     p.conn.PeerID,
			"roomId":            p.conn.RoomID,
			"serviceName":       "telemost",
			"credentials":       p.conn.Credentials,
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
			select {
			case p.reconnectCh <- struct{}{}:
			default:
			}
			return
		}
		
		p.wsMu.Lock()
		if p.ws != nil {
			if err := p.ws.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
				log.Printf("Error setting read deadline: %v", err)
			}
		}
		p.wsMu.Unlock()

		uid, _ := msg["uid"].(string)

		if _, ok := msg["serverHello"]; ok {
			p.sendAck(uid)
		}

		if _, ok := msg["updateDescription"]; ok {
			p.sendAck(uid)
		}

		if _, ok := msg["vadActivity"]; ok {
			p.sendAck(uid)
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
			if err := p.ws.WriteJSON(map[string]interface{}{
				"uid": uuid.New().String(),
				"subscriberSdpAnswer": map[string]interface{}{
					"pcSeq": int(pcSeq),
					"sdp":   answer.SDP,
				},
			}); err != nil {
				log.Printf("Error writing subscriber SDP answer: %v", err)
			}
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
			if err := p.ws.WriteJSON(map[string]interface{}{
				"uid": uuid.New().String(),
				"publisherSdpOffer": map[string]interface{}{
					"pcSeq": 1,
					"sdp":   pubOffer.SDP,
				},
			}); err != nil {
				log.Printf("Error writing publisher SDP offer: %v", err)
			}
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

	switch target {
	case "SUBSCRIBER":
		if err := p.pcSub.AddICECandidate(init); err != nil {
			log.Printf("Error adding ICE candidate to subscriber: %v", err)
		}
	case "PUBLISHER":
		if err := p.pcPub.AddICECandidate(init); err != nil {
			log.Printf("Error adding ICE candidate to publisher: %v", err)
		}
	}
}

func (p *Peer) sendAck(uid string) {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	
	if err := p.ws.WriteJSON(map[string]interface{}{
		"uid": uid,
		"ack": map[string]interface{}{
			"status": map[string]interface{}{
				"code": "OK",
			},
		},
	}); err != nil {
		log.Printf("Error writing ACK: %v", err)
	}
}

func (p *Peer) sendPong(uid string) {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	
	if err := p.ws.WriteJSON(map[string]interface{}{
		"uid": uid,
		"pong": map[string]interface{}{},
	}); err != nil {
		log.Printf("Error writing PONG: %v", err)
	}
}

func (p *Peer) setupICEHandlers() {
	p.pcSub.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}

		init := c.ToJSON()
		p.wsMu.Lock()
		if err := p.ws.WriteJSON(map[string]interface{}{
			"uid": uuid.New().String(),
			"webrtcIceCandidate": map[string]interface{}{
				"candidate":    init.Candidate,
				"sdpMid":       init.SDPMid,
				"sdpMlineIndex": init.SDPMLineIndex,
				"target":       "SUBSCRIBER",
				"pcSeq":        1,
			},
		}); err != nil {
			log.Printf("Error writing ICE candidate: %v", err)
		}
		p.wsMu.Unlock()
	})

	p.pcPub.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}

		init := c.ToJSON()
		p.wsMu.Lock()
		if err := p.ws.WriteJSON(map[string]interface{}{
			"uid": uuid.New().String(),
			"webrtcIceCandidate": map[string]interface{}{
				"candidate":    init.Candidate,
				"sdpMid":       init.SDPMid,
				"sdpMlineIndex": init.SDPMLineIndex,
				"target":       "PUBLISHER",
				"pcSeq":        1,
			},
		}); err != nil {
			log.Printf("Error writing ICE candidate: %v", err)
		}
		p.wsMu.Unlock()
	})
}

func (p *Peer) sendLeave() {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	
	if p.ws == nil {
		log.Println("WebSocket already closed, cannot send leave")
		return
	}
	
	leave := map[string]interface{}{
		"uid":   uuid.New().String(),
		"leave": map[string]interface{}{},
	}
	
	if err := p.ws.WriteJSON(leave); err != nil {
		log.Printf("Failed to send leave: %v", err)
	} else {
		log.Println("Sent leave message to server")
	}
}

func (p *Peer) Close() error {
	log.Println("Closing peer connection...")
	
	p.sendQueueClosed.Store(true)
	
	log.Println("Sending leave message...")
	p.sendLeave()
	
	time.Sleep(1 * time.Second)
	
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
		if err := p.dc.Close(); err != nil {
			log.Printf("Error closing DataChannel: %v", err)
		}
	}
	
	if p.pcPub != nil {
		log.Println("Closing Publisher PeerConnection...")
		if err := p.pcPub.Close(); err != nil {
			log.Printf("Error closing Publisher PeerConnection: %v", err)
		}
	}
	
	if p.pcSub != nil {
		log.Println("Closing Subscriber PeerConnection...")
		if err := p.pcSub.Close(); err != nil {
			log.Printf("Error closing Subscriber PeerConnection: %v", err)
		}
	}
	
	if p.ws != nil {
		log.Println("Closing WebSocket...")
		p.wsMu.Lock()
		if err := p.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)); err != nil {
			log.Printf("Error writing close message: %v", err)
		}
		if err := p.ws.Close(); err != nil {
			log.Printf("Error closing WebSocket: %v", err)
		}
		p.wsMu.Unlock()
	}
	
	log.Println("Peer closed")
	return nil
}

func (p *Peer) keepAlive() {
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
					select {
					case p.reconnectCh <- struct{}{}:
					default:
					}
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
					select {
					case p.reconnectCh <- struct{}{}:
					default:
					}
					return
				}
			}
			p.wsMu.Unlock()
		case <-p.keepAliveCh:
			return
		case <-p.closeCh:
			return
		}
	}
}

func (p *Peer) reconnect(ctx context.Context) error {
	log.Println("Reconnecting...")
	
	p.sendLeave()
	time.Sleep(500 * time.Millisecond)
	
	close(p.keepAliveCh)
	
	if p.dc != nil {
		if err := p.dc.Close(); err != nil {
			log.Printf("Error closing DataChannel: %v", err)
		}
	}
	
	if p.pcPub != nil {
		if err := p.pcPub.Close(); err != nil {
			log.Printf("Error closing Publisher PeerConnection: %v", err)
		}
	}
	
	if p.pcSub != nil {
		if err := p.pcSub.Close(); err != nil {
			log.Printf("Error closing Subscriber PeerConnection: %v", err)
		}
	}
	
	if p.ws != nil {
		p.wsMu.Lock()
		if err := p.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)); err != nil {
			log.Printf("Error writing close message: %v", err)
		}
		if err := p.ws.Close(); err != nil {
			log.Printf("Error closing WebSocket: %v", err)
		}
		p.wsMu.Unlock()
	}
	
	time.Sleep(3 * time.Second)
	
	p.keepAliveCh = make(chan struct{})
	
	conn, err := GetConnectionInfo(p.roomURL, p.name)
	if err != nil {
		return err
	}
	p.conn = conn
	
	if err := p.Connect(ctx); err != nil {
		return err
	}
	
	if p.onReconnect != nil {
		p.onReconnect(p.dc)
	}
	
	return nil
}

func (p *Peer) SetReconnectCallback(cb func(*webrtc.DataChannel)) {
	p.onReconnect = cb
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

func (p *Peer) processSendQueue(workerID int) {
	log.Printf("[WORKER-%d] Started", workerID)
	defer log.Printf("[WORKER-%d] Stopped", workerID)
	
	for {
		select {
		case data := <-p.sendQueue:
			if p.dc == nil || p.dc.ReadyState() != webrtc.DataChannelStateOpen {
				continue
			}
			
			start := time.Now()
			
			for p.dc.BufferedAmount() > 4*1024*1024 {
				time.Sleep(10 * time.Millisecond)
				if time.Since(start) > 10*time.Second {
					log.Printf("[WORKER-%d] Buffer wait timeout, dropping packet size=%d", workerID, len(data))
					break
				}
			}
			
			if time.Since(start) > 10*time.Second {
				continue
			}
			
			if err := p.dc.Send(data); err != nil {
				log.Printf("[WORKER-%d] Send error: %v", workerID, err)
			} else if time.Since(start) > 100*time.Millisecond {
				log.Printf("[WORKER-%d] Sent %d bytes in %v (buffered: %d)", 
					workerID, len(data), time.Since(start), p.dc.BufferedAmount())
			}
			
		case <-p.closeCh:
			return
		}
	}
}

func (p *Peer) monitorQueue() {
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
