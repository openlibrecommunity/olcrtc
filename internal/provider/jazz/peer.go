// Package jazz implements the SaluteJazz WebRTC provider.
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
	// maxDataChannelMessageSize is the Jazz SFU hard cap on a single
	// DataChannel message. It is DC-specific, but also represents the upper
	// bound on what the mux layer will ask any engine to transmit in a
	// single Send() call; visual engines will internally fragment further.
	maxDataChannelMessageSize = 12288

	// sendDelay is inserted between DataChannel.Send calls to avoid
	// tripping SFU rate limiters. Kept inside the package for engines that
	// need similar pacing on other transports.
	sendDelay = 2 * time.Millisecond
)

// Peer is the Jazz signaling core. It owns the WebSocket to the Jazz connector
// (SDP / ICE exchange), the pair of PeerConnections, and the reconnect
// lifecycle. The *media* layer — how application bytes actually cross a
// WebRTC session — is delegated to a mediaEngine so that different transports
// (DataChannel, VideoTrack steganography, ...) can plug into the same
// signaling code without copy-pasting 400 lines of WebSocket handling.
//
// Invariant: Peer never touches webrtc.DataChannel directly. All send/receive
// paths go through p.engine.
type Peer struct {
	name     string
	roomInfo *RoomInfo

	// WebSocket to the Jazz connector service used for the control plane:
	// SDP offers/answers and ICE candidates. Guarded by wsMu because both
	// the signaling goroutine and the send paths may write to it.
	ws   *websocket.Conn
	wsMu sync.Mutex

	// Publisher / subscriber PeerConnections. pcPub is used to send media
	// to the SFU; pcSub is used to receive the mirrored stream back.
	pcPub *webrtc.PeerConnection
	pcSub *webrtc.PeerConnection

	// engine is the pluggable media transport. It installs its media objects
	// on the two PCs during setup() and carries all subsequent byte traffic.
	engine mediaEngine

	// onData is the upward callback supplied by the provider wrapper; every
	// decoded payload from the engine fans out here.
	onData func([]byte)

	// Reconnect / lifecycle machinery. These predate the refactor and are
	// unchanged to avoid altering reconnect semantics.
	onReconnect     func(*webrtc.DataChannel)
	shouldReconnect func() bool
	reconnectCh     chan struct{}
	closeCh         chan struct{}
	closed          atomic.Bool
	reconnecting    atomic.Bool
	onEnded         func(string)
	sessionCloseCh  chan struct{}
	wg              sync.WaitGroup
	groupID         string
	selfIdentity    string
	selfParticipantSID string
	pendingTrackCID string
	subscribedTrack map[string]struct{}
	ownTrackSID     map[string]struct{}
	pingLoopOnce    sync.Once
	publisherStarted bool
}

// NewPeer creates a Jazz peer wrapping the legacy DataChannel transport.
//
// This constructor keeps the pre-refactor behaviour and API for existing
// callers (cmd/olcrtc, mobile bindings, tests). Future transports will use
// newPeerWithEngine directly with their own engine.
func NewPeer(ctx context.Context, roomID, name string, onData func([]byte)) (*Peer, error) {
	return newPeerWithEngine(ctx, roomID, name, onData, newDCEngine())
}

// NewVisualPeer creates a Jazz peer wired to the visual transport engine.
// The signaling core is identical to NewPeer; only the media engine differs.
func NewVisualPeer(ctx context.Context, roomID, name string, onData func([]byte)) (*Peer, error) {
	return newPeerWithEngine(ctx, roomID, name, onData, newVisualEngine())
}

// newPeerWithEngine is the generalised constructor that takes an arbitrary
// mediaEngine. Exported identifiers are deliberately unchanged; this one is
// package-private because it is an internal extension point for jazz-visual
// (to be added in a later stage).
func newPeerWithEngine(
	ctx context.Context,
	roomID, name string,
	onData func([]byte),
	engine mediaEngine,
) (*Peer, error) {
	var roomInfo *RoomInfo
	var err error

	// Room bootstrap: either we are asked to create a fresh room (operator
	// use case) or to join an existing one by "roomID:password" shorthand.
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
		engine:         engine,
		onData:         onData,
		reconnectCh:    make(chan struct{}, 1),
		closeCh:        make(chan struct{}),
		sessionCloseCh: make(chan struct{}),
		subscribedTrack: make(map[string]struct{}),
		ownTrackSID:    make(map[string]struct{}),
	}, nil
}

// Connect performs the full Jazz handshake:
//  1. build both PeerConnections with an (initially empty) ICE config;
//  2. let the media engine install its channels so SDP offers include them;
//  3. dial the Jazz WebSocket and send the join message;
//  4. start the signaling goroutine that drives SDP / ICE;
//  5. wait until the engine reports "ready" (transport open) or the context
//     is cancelled.
//
// The flow mirrors the pre-refactor sequence verbatim so that reconnect
// behaviour and race windows are unchanged.
//
//nolint:cyclop // handshake sequence is intentionally linear and mirrors legacy behavior
func (p *Peer) Connect(ctx context.Context) error {
	p.closed.Store(false)

	config := webrtc.Configuration{
		ICEServers:   []webrtc.ICEServer{},
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
		BundlePolicy: webrtc.BundlePolicyMaxBundle,
	}

	// Apply Android VPN socket protection if the embedder supplied one.
	// Unchanged from the previous implementation.
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

	// Wire the engine → Peer callbacks before setup so that a fast OnOpen
	// cannot race us.
	p.engine.setOnPayload(p.onEnginePayload)
	p.engine.setOnClosed(func() {
		// Preserve pre-refactor "only reconnect when we have not been told
		// to close" semantics. closed is set by Close() before anything
		// else, so racy callbacks after Close are ignored.
		if !p.closed.Load() {
			p.queueReconnect()
		}
	})

	engineReady, err := p.engine.setup(p.pcPub, p.pcSub)
	if err != nil {
		return err
	}

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

	// 30-second timeout matches the pre-refactor dcReady deadline. If the
	// transport cannot open inside that window we surface a typed error so
	// the client can decide whether to retry.
	select {
	case <-engineReady:
		return nil
	case <-time.After(30 * time.Second):
		return provider.ErrDataChannelTimeout
	case <-ctx.Done():
		return fmt.Errorf("connect cancelled: %w", ctx.Err())
	}
}

// onEnginePayload is a thin forwarder from engine → provider-level onData. It
// exists only so the engine never captures p.onData directly; Peer owns the
// policy of whether to deliver empty payloads, nil-callback guarding, etc.
func (p *Peer) onEnginePayload(payload []byte) {
	if p.onData != nil && len(payload) > 0 {
		p.onData(payload)
	}
}

func (p *Peer) dialWebSocket() error {
	// Use the VPN-protected dialer so the WebSocket connection itself
	// bypasses any Android VPN route that would otherwise loop it back.
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
	// Protocol message expected by the Jazz connector. Supported-features
	// map mirrors what the official client advertises; dropping entries
	// tends to make the SFU fall back to degraded behaviour.
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
	if err := p.ws.WriteJSON(joinMsg); err != nil {
		return fmt.Errorf("write join json: %w", err)
	}
	return nil
}

// handleSignaling owns the read side of the Jazz WebSocket. It blocks on
// ReadJSON and dispatches each message to a topic-specific handler. A read
// error triggers a reconnect request unless Peer is already closing.
func (p *Peer) handleSignaling(_ context.Context) {
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
			p.handleMediaOut(payload)
		case "error":
			logger.Debugf("Jazz error event: %+v", payload)
		default:
			logger.Debugf("Jazz unhandled event=%s payload=%+v", event, payload)
		}
	}
}

func (p *Peer) handleJoinResponse(payload map[string]any) {
	// participantGroup.groupId is echoed back in every subsequent media-in
	// message so the SFU can route us to the right session subgroup.
	if participant, ok := payload["participant"].(map[string]any); ok {
		if identity, _ := participant["participantId"].(string); identity != "" {
			p.selfIdentity = identity
		}
	}
	group, _ := payload["participantGroup"].(map[string]any)
	p.groupID, _ = group["groupId"].(string)
	logger.Verbosef("Jazz peer joined: groupId=%s", p.groupID)
	p.startRTCPingLoop()
}

func (p *Peer) handleMediaOut(payload map[string]any) {
	// media-out carries all WebRTC-layer messages from the SFU. We multiplex
	// on the "method" field rather than on the top-level event so that new
	// method types (e.g. renegotiation) can be added without disturbing the
	// outer loop.
	method, _ := payload["method"].(string)

	// Unknown and no-op methods (e.g. rtc:pong) are silently dropped.
	switch method {
	case "rtc:config":
		p.handleRTCConfig(payload)
	case "rtc:join":
		logger.Verbosef("Jazz rtc:join received")
		p.handleRTCJoin(payload)
	case "rtc:offer":
		p.handleSubscriberOffer(payload)
	case "rtc:answer":
		p.handlePublisherAnswer(payload)
	case "rtc:ice":
		p.handleICE(payload)
	case "rtc:track:published":
		p.handleTrackPublished(payload)
	case "rtc:participants:update":
		p.handleParticipantsUpdate(payload)
	case "rtc:ping":
		p.handleRTCPing(payload)
	case "rtc:pong":
		// SFU pong response: no action needed.
	}
}

func (p *Peer) startRTCPingLoop() {
	p.pingLoopOnce.Do(func() {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-p.closeCh:
					return
				case <-ticker.C:
					if p.closed.Load() || p.groupID == "" {
						continue
					}
					p.sendRTCPing(0)
				}
			}
		}()
	})
}

func (p *Peer) sendRTCPing(rtt int64) {
	p.wsMu.Lock()
	_ = p.ws.WriteJSON(map[string]any{
		"roomId":    p.roomInfo.RoomID,
		"event":     "media-in",
		"groupId":   p.groupID,
		"requestId": uuid.New().String(),
		"payload": map[string]any{
			"method": "rtc:ping",
			"ping_req": map[string]any{
				"timestamp": time.Now().UnixMilli(),
				"rtt":       rtt,
			},
		},
	})
	p.wsMu.Unlock()
}

func (p *Peer) handleRTCPing(payload map[string]any) {
	pingReq, _ := payload["ping_req"].(map[string]any)
	lastPingTimestamp := pingReq["timestamp"]

	p.wsMu.Lock()
	_ = p.ws.WriteJSON(map[string]any{
		"roomId":    p.roomInfo.RoomID,
		"event":     "media-in",
		"groupId":   p.groupID,
		"requestId": uuid.New().String(),
		"payload": map[string]any{
			"method": "rtc:pong",
			"pong_resp": map[string]any{
				"lastPingTimestamp": lastPingTimestamp,
				"timestamp":         time.Now().UnixMilli(),
			},
		},
	})
	p.wsMu.Unlock()
}


func (p *Peer) handleParticipantsUpdate(payload map[string]any) {
	logger.Debugf("Jazz unhandled media-out method=rtc:participants:update payload=%+v", payload)

	update, _ := payload["update"].(map[string]any)
	participants, _ := update["participants"].([]any)
	p.subscribeParticipantList(participants)
}

func (p *Peer) handleRTCJoin(payload map[string]any) {
	join, _ := payload["join"].(map[string]any)
	participant, _ := join["participant"].(map[string]any)
	if identity, _ := participant["identity"].(string); identity != "" {
		p.selfIdentity = identity
	}
	if sid, _ := participant["sid"].(string); sid != "" {
		p.selfParticipantSID = sid
	}
	others, _ := join["otherParticipants"].([]any)
	p.subscribeParticipantList(others)
}

func (p *Peer) subscribeParticipantList(participants []any) {
	for _, item := range participants {
		participant, _ := item.(map[string]any)
		identity, _ := participant["identity"].(string)
		participantSID, _ := participant["sid"].(string)
		if identity != "" && identity == p.selfIdentity {
			continue
		}
		if participantSID != "" && participantSID == p.selfParticipantSID {
			continue
		}
		tracks, _ := participant["tracks"].([]any)
		p.subscribeParticipantTracks(tracks)
	}
}

func (p *Peer) subscribeParticipantTracks(tracks []any) {
	for _, trackItem := range tracks {
		track, _ := trackItem.(map[string]any)
		trackType, _ := track["type"].(string)
		trackSID, _ := track["sid"].(string)
		if trackType != "VIDEO" || trackSID == "" {
			continue
		}
		if _, own := p.ownTrackSID[trackSID]; own {
			continue
		}
		if _, seen := p.subscribedTrack[trackSID]; seen {
			continue
		}
		p.subscribedTrack[trackSID] = struct{}{}
		p.sendTrackSubscriptionChange(trackSID, true)
		p.sendTrackSettingsUpdate(trackSID, 640, 480)
	}
}

func (p *Peer) handleRTCConfig(payload map[string]any) {
	// Re-parse the ICE server list from the SFU-provided config and apply it
	// to both PCs. Anything malformed is silently dropped so a single bad
	// entry cannot poison the whole set.
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

func (p *Peer) handleSubscriberOffer(payload map[string]any) {
	// The SFU sends us an offer for the subscriber PC first; we answer it
	// and *then* initiate our own publisher offer. The 300ms sleep between
	// the answer and the publisher offer is load-bearing: shorter delays
	// occasionally cause the SFU to bounce the publisher SDP. Inherited
	// from the original code.
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

	if !p.publisherStarted {
		p.publisherStarted = true
		time.Sleep(300 * time.Millisecond)
		p.startPublisherOffer()
	}
}

func (p *Peer) startPublisherOffer() {
	if track := p.resolveNextTrackMeta(); len(track) > 0 {
		p.sendTrackAdd(track)
		return
	}
	p.sendPublisherOffer()
}

// resolveNextTrackMeta returns the next track metadata from the engine,
// preferring the sequenced path (publisherTrackSequencer) over the simple
// one-shot path (trackMetadataProvider).
func (p *Peer) resolveNextTrackMeta() map[string]any {
	if sequencer, ok := p.engine.(publisherTrackSequencer); ok {
		return sequencer.nextTrackMetadata()
	}
	if provider, ok := p.engine.(trackMetadataProvider); ok {
		return provider.trackMetadata()
	}
	return nil
}

// sendTrackAdd sends an rtc:track:add media-in message for the given track
// metadata and records its cid as a pending publisher track.
func (p *Peer) sendTrackAdd(track map[string]any) {
	if cid, _ := track["cid"].(string); cid != "" {
		p.pendingTrackCID = cid
	}
	p.wsMu.Lock()
	_ = p.ws.WriteJSON(map[string]any{
		"roomId":    p.roomInfo.RoomID,
		"event":     "media-in",
		"groupId":   p.groupID,
		"requestId": uuid.New().String(),
		"payload": map[string]any{
			"method":          "rtc:track:add",
			"addTrackRequest": track,
		},
	})
	p.wsMu.Unlock()
	logger.Debugf("rtc:track:add sent: %+v", track)
}

func (p *Peer) handleTrackPublished(payload map[string]any) {
	logger.Debugf("Jazz rtc:track:published received: %+v", payload)

	resp, _ := payload["trackPublishedResponse"].(map[string]any)
	cid, _ := resp["cid"].(string)
	track, _ := resp["track"].(map[string]any)
	sid, _ := track["sid"].(string)
	logger.Debugf("Jazz track published: cid=%s sid=%s", cid, sid)
	if sid != "" {
		p.ownTrackSID[sid] = struct{}{}
	}

	if sequencer, ok := p.engine.(publisherTrackSequencer); ok {
		sequencer.onTrackPublished(cid, sid)
	}

	if p.pendingTrackCID != "" && cid == p.pendingTrackCID {
		p.pendingTrackCID = ""
		p.sendPublisherOffer()
	}
}

func (p *Peer) sendTrackSubscriptionChange(trackSID string, subscribe bool) {
	p.wsMu.Lock()
	_ = p.ws.WriteJSON(map[string]any{
		"roomId":    p.roomInfo.RoomID,
		"event":     "media-in",
		"groupId":   p.groupID,
		"requestId": uuid.New().String(),
		"payload": map[string]any{
			"method": "rtc:track:subscription:change",
			"subscription": map[string]any{
				"trackSids": []string{trackSID},
				"subscribe": subscribe,
			},
		},
	})
	p.wsMu.Unlock()
	logger.Debugf("rtc:track:subscription:change sent: sid=%s subscribe=%t", trackSID, subscribe)
}

func (p *Peer) sendTrackSettingsUpdate(trackSID string, width, height int) {
	p.wsMu.Lock()
	_ = p.ws.WriteJSON(map[string]any{
		"roomId":    p.roomInfo.RoomID,
		"event":     "media-in",
		"groupId":   p.groupID,
		"requestId": uuid.New().String(),
		"payload": map[string]any{
			"method": "rtc:track:settings:update",
			"trackSetting": map[string]any{
				"trackSids": []string{trackSID},
				"disabled":  false,
				"width":     width,
				"height":    height,
			},
		},
	})
	p.wsMu.Unlock()
	logger.Debugf("rtc:track:settings:update sent: sid=%s %dx%d", trackSID, width, height)
}

func (p *Peer) sendPublisherOffer() {
	if trackAttacher, ok := p.engine.(publisherTrackAttacher); ok {
		if err := trackAttacher.attachPublisherTrack(p.pcPub); err != nil {
			logger.Debugf("attach publisher track error: %v", err)
			return
		}
	}

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
		return
	}

	if sequencer, ok := p.engine.(publisherTrackSequencer); ok && !sequencer.publisherReady() {
		p.startPublisherOffer()
		return
	}

	if notifier, ok := p.engine.(publisherReadyNotifier); ok {
		notifier.markPublisherReady()
	}
}

func (p *Peer) handleICE(payload map[string]any) {
	// Trickle ICE: the SFU streams candidates as it gathers them. Each
	// candidate is tagged with its target PC so we know whether to feed
	// subscriber or publisher.
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

// Send queues data for transmission. Thin delegation to the engine so that
// the provider.Provider interface surface stays stable across transports.
func (p *Peer) Send(data []byte) error {
	return p.engine.send(data)
}

// Close terminates the connection and releases resources. Order matters:
// stop accepting new sends, wait for in-flight signaling goroutines, close
// the engine (which may take up to 2s to drain), close both PCs, then close
// the WebSocket with a clean frame.
func (p *Peer) Close() error {
	p.closed.Store(true)

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

	if p.engine != nil {
		_ = p.engine.close()
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

// SetReconnectCallback sets the callback for reconnection events.
func (p *Peer) SetReconnectCallback(cb func(*webrtc.DataChannel)) {
	p.onReconnect = cb
}

// SetShouldReconnect sets the policy for reconnection.
func (p *Peer) SetShouldReconnect(fn func() bool) {
	p.shouldReconnect = fn
}

// SetEndedCallback sets the callback for connection termination.
func (p *Peer) SetEndedCallback(cb func(string)) {
	p.onEnded = cb
}

// WatchConnection blocks until the peer is closed or a reconnect is
// requested. The client's reconnect driver loops on this channel so it can
// schedule retries without polling.
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

// CanSend reports whether a send call is likely to succeed immediately.
// Delegates to the engine; kept on Peer to match the provider.Provider shape.
func (p *Peer) CanSend() bool {
	return p.engine.canSend()
}

// GetSendQueue returns the transmission queue. Pre-refactor callers read
// len(queue) for back-pressure heuristics; we expose the engine's internal
// channel directly so those readings stay consistent.
func (p *Peer) GetSendQueue() chan []byte {
	return p.engine.sendQueue()
}

// GetBufferedAmount returns the transport-level buffered byte count.
func (p *Peer) GetBufferedAmount() uint64 {
	return p.engine.bufferedAmount()
}

// queueReconnect is idempotent: multiple parallel failure signals collapse
// into a single pending reconnect. The shouldReconnect policy lets higher
// layers veto reconnects (e.g. during orderly shutdown).
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
