package goolom

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"

	"github.com/openlibrecommunity/olcrtc/internal/engine"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
)

func (s *Session) setupPeerConnections(config webrtc.Configuration) error {
	api, err := newWebRTCAPI(s.resolver)
	if err != nil {
		return err
	}

	sub, err := api.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("new sub pc: %w", err)
	}
	sub.OnConnectionStateChange(s.onSubscriberConnectionStateChange)
	sub.OnTrack(s.onSubscriberTrack)
	s.pcSub.Store(sub)

	pub, err := api.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("new pub pc: %w", err)
	}
	pub.OnConnectionStateChange(s.onPublisherConnectionStateChange)
	s.pcPub.Store(pub)

	return s.attachPendingVideoTracks(pub)
}

// newWebRTCAPI builds a pion API with IPv4-only ICE and default interceptors.
func newWebRTCAPI(resolver *net.Resolver) (*webrtc.API, error) {
	settingEngine := webrtc.SettingEngine{}
	apply, err := engine.NewPionSettings(engine.PionSettingsOptions{
		Resolver:         resolver,
		LoggerFactory:    logger.NewPionLoggerFactory(),
		IPv4Only:         true,
		ProxyDialer:      true,
		DisableMulticast: true,
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // shared builder already adds protected-net context
	}
	apply(&settingEngine)

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("register default codecs: %w", err)
	}
	interceptorRegistry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		return nil, fmt.Errorf("register default interceptors: %w", err)
	}
	return webrtc.NewAPI(
		webrtc.WithSettingEngine(settingEngine),
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
	), nil
}

func (s *Session) onSubscriberTrack(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	if track.Kind() != webrtc.RTPCodecTypeVideo {
		return
	}
	logger.Infof("goolom remote video track: codec=%s stream=%s track=%s",
		track.Codec().MimeType, track.StreamID(), track.ID())
	if cb := s.videoTrackHandler(); cb != nil {
		cb(track, receiver)
	}
	go engine.DrainRTCP(receiver)
}

func (s *Session) setupDataChannelHandlers(
	dc *webrtc.DataChannel, dcReady chan struct{}, sessionCloseCh chan struct{},
) {
	dc.OnOpen(func() {
		numWorkers := 4
		for i := range numWorkers {
			s.goLaunch(func() { s.processSendQueue(dc, i, sessionCloseCh) })
		}
		close(dcReady)
	})

	dc.OnClose(s.onDataChannelClose)
	dc.OnMessage(s.onDataChannelMessage)

	s.subPC().OnDataChannel(func(remote *webrtc.DataChannel) {
		if s.onData != nil {
			remote.OnMessage(s.onDataChannelMessage)
		}
	})
}

func (s *Session) onDataChannelClose() {
	if !s.closed.Load() {
		s.queueReconnect()
	}
}

func (s *Session) onDataChannelMessage(msg webrtc.DataChannelMessage) {
	if s.onData != nil && len(msg.Data) > 0 {
		s.onData(msg.Data)
	}
}

func (s *Session) handleSdpOffer(offer map[string]any, uid string, sendPub bool) error {
	sub := s.subPC()
	if sub == nil {
		return ErrSessionClosed
	}
	sdp, _ := offer["sdp"].(string)
	pcSeq, _ := offer["pcSeq"].(float64)

	if err := sub.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}); err != nil {
		return fmt.Errorf("set remote desc: %w", err)
	}

	answer, err := sub.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create answer: %w", err)
	}

	if setErr := sub.SetLocalDescription(answer); setErr != nil {
		return fmt.Errorf("set local desc: %w", setErr)
	}

	if writeErr := s.writeJSON(map[string]any{
		keyUID: uuid.New().String(),
		"subscriberSdpAnswer": map[string]any{
			keyPcSeq: int(pcSeq),
			"sdp":    answer.SDP,
		},
	}); writeErr != nil {
		return fmt.Errorf("send subscriber answer: %w", writeErr)
	}

	s.sendAck(uid)

	if s.onData == nil {
		if slotErr := s.sendSetSlots(); slotErr != nil {
			logger.Debugf("setSlots error: %v", slotErr)
		}
	}

	if !sendPub {
		return nil
	}

	// Give the SFU time to apply our subscriberSdpAnswer before the
	// publisher offer lands on the same signaling channel: in SEPARATE
	// offer/answer mode it drops a publisher offer that arrives while the
	// subscriber negotiation for the same peer is still being processed.
	//
	// The precise condition is the server's ack for the answer we just
	// wrote, and the ack machinery exists (registerAckWaiter/waitForAck) -
	// but it cannot be used here: handleSdpOffer runs on the single
	// signaling read goroutine, so blocking for the ack would block the
	// very loop that has to read it. This is a one-shot delay (sendPub is
	// true only for the first subscriber offer of a session).
	time.Sleep(300 * time.Millisecond)

	pub := s.pubPC()
	if pub == nil {
		return ErrPublisherNotInitialized
	}
	pubOffer, err := pub.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create pub offer: %w", err)
	}
	if err := pub.SetLocalDescription(pubOffer); err != nil {
		return fmt.Errorf("set local pub desc: %w", err)
	}

	if err := s.writeJSON(map[string]any{
		keyUID: uuid.New().String(),
		"publisherSdpOffer": map[string]any{
			keyPcSeq: 1,
			"sdp":    pubOffer.SDP,
			"tracks": s.publisherTrackDescriptions(),
		},
	}); err != nil {
		return fmt.Errorf("send publisher offer: %w", err)
	}
	return nil
}

func (s *Session) handleSdpAnswer(answer map[string]any, uid string) {
	pub := s.pubPC()
	if pub == nil {
		return
	}
	sdp, _ := answer["sdp"].(string)
	if err := pub.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	}); err != nil {
		// ai-generated: a failed SetRemoteDescription leaves the publisher
		// PC's ICE agent without a remote ufrag/pwd, so it silently stays in
		// "new" and never reaches Connecting - nothing noticed until the
		// outer readiness timeout (tens of seconds) finally gave up. Closing
		// it here (off this goroutine, since Close can block on TURN
		// deallocation) routes through onPublisherConnectionStateChange's
		// existing Closed handling, which already triggers a fast,
		// well-tested reconnect instead of a silent multi-second hang.
		logger.Warnf("goolom publisher SetRemoteDescription failed: %v", err)
		s.goLaunch(func() { _ = pub.Close() })
	}
	s.sendAck(uid)
}

func (s *Session) handleICE(cand map[string]any) {
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
		if sub := s.subPC(); sub != nil {
			_ = sub.AddICECandidate(init)
		}
	case "PUBLISHER":
		if pub := s.pubPC(); pub != nil {
			_ = pub.AddICECandidate(init)
		}
	}
}

func (s *Session) setupICEHandlers() {
	if sub := s.subPC(); sub != nil {
		sub.OnICECandidate(s.iceCandidateHandler("SUBSCRIBER"))
	}
	if pub := s.pubPC(); pub != nil {
		pub.OnICECandidate(s.iceCandidateHandler("PUBLISHER"))
	}
}

// iceCandidateHandler builds the OnICECandidate callback that forwards local
// candidates to the SFU for the given target side.
func (s *Session) iceCandidateHandler(target string) func(*webrtc.ICECandidate) {
	return func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		if err := s.writeJSON(map[string]any{
			keyUID: uuid.New().String(),
			"webrtcIceCandidate": map[string]any{
				"candidate":     init.Candidate,
				"sdpMid":        init.SDPMid,
				"sdpMlineIndex": init.SDPMLineIndex,
				"target":        target,
				keyPcSeq:        1,
			},
		}); err != nil {
			logger.Debugf("goolom: ice candidate (%s): %v", target, err)
		}
	}
}

func (s *Session) sendSetSlots() error {
	// Goolom only forwards as many remote videos as the subscriber asks for via
	// setSlots. Request a generous count so each subscriber sees every active
	// publisher in the room.
	slots := make([]map[string]int, 0, 8)
	for range 8 {
		slots = append(slots, map[string]int{"width": 1280, "height": 720})
	}
	if err := s.writeJSON(map[string]any{
		keyUID: uuid.New().String(),
		"setSlots": map[string]any{
			"slots":              slots,
			"audioSlotsCount":    0,
			"key":                1,
			"shutdownAllVideo":   nil,
			"withSelfView":       false,
			"selfViewVisibility": "ON_LOADING_THEN_SHOW",
			"gridConfig":         map[string]any{},
		},
	}); err != nil {
		return fmt.Errorf("write set slots: %w", err)
	}
	return nil
}

func (s *Session) publisherTrackDescriptions() []map[string]any {
	pub := s.pubPC()
	if pub == nil {
		return nil
	}
	tracks := make([]map[string]any, 0)
	for _, transceiver := range pub.GetTransceivers() {
		sender := transceiver.Sender()
		if sender == nil {
			continue
		}
		track := sender.Track()
		if track == nil {
			continue
		}
		kind := "VIDEO"
		if track.Kind() == webrtc.RTPCodecTypeAudio {
			kind = "AUDIO"
		}
		tracks = append(tracks, map[string]any{
			"mid":            transceiver.Mid(),
			"transceiverMid": transceiver.Mid(),
			"kind":           kind,
			"priority":       0,
			"label":          track.ID(),
			"codecs":         map[string]any{},
			"groupId":        1,
			keyDescription:   "",
		})
	}
	return tracks
}

func parseICEURLs(server map[string]any) []string {
	var urls []string
	switch rawURLs := server["urls"].(type) {
	case []any:
		for _, rawURL := range rawURLs {
			if url, ok := rawURL.(string); ok {
				urls = append(urls, url)
			}
		}
	case []string:
		urls = append(urls, rawURLs...)
	}
	return urls
}

func parseICEServer(rawServer any) (webrtc.ICEServer, bool) {
	server, ok := rawServer.(map[string]any)
	if !ok {
		return webrtc.ICEServer{}, false
	}
	urls := parseICEURLs(server)
	if len(urls) == 0 {
		return webrtc.ICEServer{}, false
	}
	ice := webrtc.ICEServer{URLs: urls}
	if username, ok := server["username"].(string); ok {
		ice.Username = username
	}
	if credential, ok := server["credential"].(string); ok {
		ice.Credential = credential
	}
	normalised := engine.NormaliseICEServers([]webrtc.ICEServer{ice})
	if len(normalised) == 0 {
		return webrtc.ICEServer{}, false
	}
	return normalised[0], true
}

func (s *Session) applyServerHelloConfig(serverHello map[string]any) {
	rawCfg, ok := serverHello["rtcConfiguration"].(map[string]any)
	if !ok {
		return
	}
	rawServers, ok := rawCfg["iceServers"].([]any)
	if !ok || len(rawServers) == 0 {
		return
	}
	iceServers := make([]webrtc.ICEServer, 0, len(rawServers))
	for _, rawServer := range rawServers {
		if ice, ok := parseICEServer(rawServer); ok {
			iceServers = append(iceServers, ice)
		}
	}
	if len(iceServers) == 0 {
		return
	}
	cfg := webrtc.Configuration{
		ICEServers:   iceServers,
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	}
	if sub := s.subPC(); sub != nil {
		_ = sub.SetConfiguration(cfg)
	}
	if pub := s.pubPC(); pub != nil {
		_ = pub.SetConfiguration(cfg)
	}
}
