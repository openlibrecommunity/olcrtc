package telemost

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
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
		ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	
	ws.SetReadDeadline(time.Now().Add(60 * time.Second))

	go p.keepAlive()

	if err := p.sendHello(); err != nil {
		return err
	}

	p.setupICEHandlers()

	go p.handleSignaling()

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
	
	for p.dc.BufferedAmount() > 256*1024 {
		time.Sleep(1 * time.Millisecond)
	}
	
	return p.dc.Send(data)
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
			p.ws.SetReadDeadline(time.Now().Add(60 * time.Second))
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
		p.pcSub.AddICECandidate(init)
	} else if target == "PUBLISHER" {
		p.pcPub.AddICECandidate(init)
	}
}

func (p *Peer) sendAck(uid string) {
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

func (p *Peer) sendPong(uid string) {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	
	p.ws.WriteJSON(map[string]interface{}{
		"uid": uid,
		"pong": map[string]interface{}{},
	})
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
				"candidate":    init.Candidate,
				"sdpMid":       init.SDPMid,
				"sdpMlineIndex": init.SDPMLineIndex,
				"target":       "SUBSCRIBER",
				"pcSeq":        1,
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
				"candidate":    init.Candidate,
				"sdpMid":       init.SDPMid,
				"sdpMlineIndex": init.SDPMLineIndex,
				"target":       "PUBLISHER",
				"pcSeq":        1,
			},
		})
		p.wsMu.Unlock()
	})
}

func (p *Peer) sendLeave() {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	
	if p.ws == nil {
		return
	}
	
	leave := map[string]interface{}{
		"uid":   uuid.New().String(),
		"leave": map[string]interface{}{},
	}
	if err := p.ws.WriteJSON(leave); err == nil {
		log.Println("Sent leave message to server")
		time.Sleep(200 * time.Millisecond)
	}
}

func (p *Peer) Close() error {
	p.sendLeave()
	
	close(p.closeCh)
	
	if p.dc != nil {
		p.dc.Close()
	}
	
	if p.pcPub != nil {
		p.pcPub.Close()
	}
	
	if p.pcSub != nil {
		p.pcSub.Close()
	}
	
	if p.ws != nil {
		p.ws.Close()
	}
	
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
	
	close(p.keepAliveCh)
	
	if p.dc != nil {
		p.dc.Close()
	}
	
	if p.pcPub != nil {
		p.pcPub.Close()
	}
	
	if p.pcSub != nil {
		p.pcSub.Close()
	}
	
	if p.ws != nil {
		p.ws.Close()
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
