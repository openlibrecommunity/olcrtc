package jitsi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strconv"
	"time"

	pioninterceptor "github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"github.com/zarazaex69/j"

	"github.com/openlibrecommunity/olcrtc/internal/engine"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

// waitForJingle waits for Jicofo's session-initiate after a peer joins.
func (s *Session) waitForJingle() {
	jSess := s.jSess.Load()
	if jSess == nil {
		return
	}

	stanza, err := jSess.Conn.WaitJingle(s.runCtx)
	if err != nil {
		if s.closed.Load() || s.runCtx.Err() != nil {
			return
		}
		logger.Warnf("jitsi: wait jingle failed: %v", err)
		s.requestReconnect("wait jingle failed: " + err.Error())
		return
	}
	_ = stanza // completeJingleSetup reads the cached stanza through j.Session.

	if err := s.completeJingleSetup(s.runCtx, jSess); err != nil {
		if !s.closed.Load() {
			logger.Warnf("jitsi: jingle setup failed: %v", err)
			s.requestReconnect("jingle setup failed")
		}
	}
}

func (s *Session) completeJingleSetup(ctx context.Context, jSess *j.Session) error {
	logger.Infof("jitsi: session-initiate received; colibri-ws=%s", jSess.ColibriWS)

	needBridge := s.onData != nil || s.onPeerData != nil
	wantVideo := s.shouldRequestVideo()
	sctpBridge := (needBridge || wantVideo) && jSess.ColibriWS == ""

	if (needBridge || wantVideo) && !sctpBridge {
		if err := s.openBridgeWS(ctx, jSess); err != nil {
			return err
		}
	}
	if s.shouldNegotiatePC(needBridge) {
		if err := s.negotiatePC(ctx, jSess, sctpBridge); err != nil {
			return err
		}
	}
	if sctpBridge {
		if err := s.openBridgeSCTP(ctx, jSess); err != nil {
			return err
		}
	}

	// JVB only forwards video after the bridge is open and RequestVideo has
	// established receiver constraints.
	if wantVideo {
		if err := jSess.RequestVideo(ctx, 720); err != nil {
			logger.Debugf("jitsi: request video: %v", err)
		}
	}

	s.goLaunch(s.recvLoop)
	s.goLaunch(func() { s.announceEpoch(needBridge) })
	return nil
}

func (s *Session) announceEpoch(needBridge bool) {
	if !needBridge {
		return
	}
	const (
		interval = 200 * time.Millisecond
		attempts = 25
	)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range attempts {
		if s.closed.Load() || s.peerEpoch.Load() != 0 {
			return
		}
		if err := s.Send(nil); err != nil {
			logger.Debugf("jitsi: epoch announce failed: %v", err)
		}
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}
	}
}

func (s *Session) shouldNegotiatePC(needBridge bool) bool {
	return needBridge || s.shouldRequestVideo()
}

func (s *Session) shouldRequestVideo() bool {
	return s.WantsVideo()
}

// newSettingEngine builds the pion settings shared with the other engines.
func newSettingEngine(resolver protect.Lookup) (webrtc.SettingEngine, error) {
	settings := webrtc.SettingEngine{}
	apply, err := engine.NewPionSettings(engine.PionSettingsOptions{
		Resolver:         resolver,
		LoggerFactory:    logger.NewPionLoggerFactory(),
		IPv4Only:         true,
		DisableMulticast: true,
	})
	if err != nil {
		return settings, err //nolint:wrapcheck // shared builder adds protected-net context
	}
	apply(&settings)
	return settings, nil
}

func newConferenceAPI(resolver protect.Lookup) (*webrtc.API, error) {
	settings, err := newSettingEngine(resolver)
	if err != nil {
		return nil, err
	}
	// JVB performs RTCP feedback aggregation, so avoid default interceptor
	// probes before DTLS starts.
	registry := &pioninterceptor.Registry{}
	return webrtc.NewAPI(
		webrtc.WithSettingEngine(settings),
		webrtc.WithInterceptorRegistry(registry),
	), nil
}

// negotiatePC applies Jicofo's offer in ordered stages. Trickle draining starts
// before Accept so source-add cannot lose a race with the first RTP packet.
//
//nolint:contextcheck // peer connection lifetime follows runCtx, not negotiation timeout
func (s *Session) negotiatePC(ctx context.Context, jSess *j.Session, sctpBridge bool) error {
	pc, err := s.newConferencePeerConnection(jSess)
	if err != nil {
		return err
	}

	hasLocalTracks, keepaliveTrack, err := s.configureConferenceTracks(pc)
	if err != nil {
		_ = pc.Close()
		return err
	}
	s.installPeerConnectionHandlers(pc)
	if err := s.preparePeerConnection(ctx, jSess, pc, sctpBridge, hasLocalTracks); err != nil {
		_ = pc.Close()
		return err
	}

	pcCtx := s.installPeerConnectionState(pc)
	if keepaliveTrack != nil {
		s.goLaunch(func() { s.rtpKeepalive(pcCtx, keepaliveTrack) })
	}
	return nil
}

func (s *Session) newConferencePeerConnection(jSess *j.Session) (*webrtc.PeerConnection, error) {
	api, err := newConferenceAPI(s.resolver)
	if err != nil {
		return nil, err
	}
	pcConfig := jSess.IceConfig()
	pcConfig.ICEServers = engine.NormaliseICEServers(pcConfig.ICEServers)
	pcConfig.SDPSemantics = webrtc.SDPSemanticsPlanB
	pc, err := api.NewPeerConnection(pcConfig)
	if err != nil {
		return nil, fmt.Errorf("new pc: %w", err)
	}
	if _, err := pc.AddTransceiverFromKind(
		webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("add audio recvonly: %w", err)
	}
	return pc, nil
}

func (s *Session) configureConferenceTracks(
	pc *webrtc.PeerConnection,
) (bool, *webrtc.TrackLocalStaticSample, error) {
	hasLocalTracks, err := s.addVideoTransceivers(pc)
	if err != nil {
		return false, nil, err
	}
	keepaliveTrack, err := s.setupVideoMLine(pc, hasLocalTracks)
	if err != nil {
		return false, nil, err
	}
	return hasLocalTracks, keepaliveTrack, nil
}

func (s *Session) installPeerConnectionHandlers(pc *webrtc.PeerConnection) {
	pc.OnTrack(s.handleRemoteTrack)
	pc.OnConnectionStateChange(s.handlePeerConnectionState)
}

func (s *Session) preparePeerConnection(
	ctx context.Context,
	jSess *j.Session,
	pc *webrtc.PeerConnection,
	sctpBridge bool,
	hasLocalTracks bool,
) error {
	neg := jSess.Negotiator()
	neg.PC = pc
	neg.OnIceConnectionStateChange = func(state webrtc.ICEConnectionState) {
		logger.Debugf("jitsi ICE state: %s", state)
	}

	trickleCtx, trickleCancel := context.WithCancel(ctx)
	s.setTrickleCancel(trickleCancel)
	stanzas := jSess.LowLevel().Stanzas()
	s.goLaunch(func() { s.trickleDrainLoop(trickleCtx, pc, neg, stanzas) })

	if sctpBridge {
		if err := jSess.PrepareBridgeSCTP(pc); err != nil {
			return fmt.Errorf("prepare bridge sctp: %w", err)
		}
	}
	if err := neg.Accept(ctx); err != nil {
		return fmt.Errorf("session-accept: %w", err)
	}
	if hasLocalTracks {
		if err := neg.SendSourceAddFromSDP(pc.LocalDescription().SDP); err != nil {
			logger.Debugf("jitsi: source-add (initial): %v", err)
		}
	}
	return nil
}

func (s *Session) installPeerConnectionState(pc *webrtc.PeerConnection) context.Context {
	s.pcMu.Lock()
	s.pc = pc
	if s.pcCancel != nil {
		s.pcCancel()
	}
	s.pcCtx, s.pcCancel = context.WithCancel(s.runCtx)
	pcCtx := s.pcCtx
	s.pcMu.Unlock()
	return pcCtx
}

func (s *Session) addVideoTransceivers(pc *webrtc.PeerConnection) (bool, error) {
	var addErr error
	hasLocalTracks := s.RangeVideoTracks(func(track webrtc.TrackLocal, wantsRemote bool) {
		if addErr != nil {
			return
		}
		direction := webrtc.RTPTransceiverDirectionSendonly
		if wantsRemote {
			direction = webrtc.RTPTransceiverDirectionSendrecv
		}
		_, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: direction})
		if err != nil {
			addErr = fmt.Errorf("add track: %w", err)
		}
	})
	return hasLocalTracks, addErr
}

func (s *Session) setupVideoMLine(
	pc *webrtc.PeerConnection,
	hasLocalTracks bool,
) (*webrtc.TrackLocalStaticSample, error) {
	if !hasLocalTracks {
		return s.addVideoOrKeepaliveTrack(pc)
	}
	return nil, nil //nolint:nilnil // no keepalive needed with local tracks
}

func (s *Session) addVideoOrKeepaliveTrack(
	pc *webrtc.PeerConnection,
) (*webrtc.TrackLocalStaticSample, error) {
	if s.wantsVideoReceive() {
		if _, err := pc.AddTransceiverFromKind(
			webrtc.RTPCodecTypeVideo,
			webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
		); err != nil {
			return nil, fmt.Errorf("add video recvonly: %w", err)
		}
		return nil, nil //nolint:nilnil // nil signals no keepalive track
	}
	keepaliveTrack, err := newKeepaliveTrack()
	if err != nil {
		return nil, fmt.Errorf("create keepalive track: %w", err)
	}
	if _, err := pc.AddTrack(keepaliveTrack); err != nil {
		return nil, fmt.Errorf("add keepalive track: %w", err)
	}
	return keepaliveTrack, nil
}

func (s *Session) wantsVideoReceive() bool {
	return s.VideoTrackHandler() != nil
}

func newKeepaliveTrack() (*webrtc.TrackLocalStaticSample, error) {
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		"jitsi-ka-"+randomTrackSuffix(),
		"olcrtc-ka-"+randomTrackSuffix(),
	)
	if err != nil {
		return nil, fmt.Errorf("new keepalive track: %w", err)
	}
	return track, nil
}

func randomTrackSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func drainTrack(track *webrtc.TrackRemote) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := track.Read(buf); err != nil {
			return
		}
	}
}

func (s *Session) handleRemoteTrack(track *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
	if track.Kind() != webrtc.RTPCodecTypeVideo {
		return
	}
	ssrc := uint32(track.SSRC())
	if !s.peerVideoSSRC.CompareAndSwap(0, ssrc) && s.peerVideoSSRC.Load() != ssrc {
		go drainTrack(track)
		return
	}
	if cb := s.VideoTrackHandler(); cb != nil {
		cb(track, recv)
	}
}

func (s *Session) handlePeerConnectionState(state webrtc.PeerConnectionState) {
	logger.Debugf("jitsi pc state: %s", state.String())
	if state == webrtc.PeerConnectionStateFailed && !s.closed.Load() {
		s.requestReconnect("jitsi peer connection failed")
	}
}

// teardownPC cancels PC-bound goroutines before closing the connection.
func (s *Session) teardownPC() {
	s.pcMu.Lock()
	oldPC := s.pc
	s.pc = nil
	pcCancel := s.pcCancel
	s.pcCancel = nil
	s.pcCtx = nil
	trickleCancel := s.trickleCancel
	s.trickleCancel = nil
	s.pcMu.Unlock()
	if pcCancel != nil {
		pcCancel()
	}
	if trickleCancel != nil {
		trickleCancel()
	}
	if oldPC != nil {
		_ = oldPC.Close()
	}
}

func (s *Session) setTrickleCancel(cancel context.CancelFunc) {
	s.pcMu.Lock()
	prev := s.trickleCancel
	s.trickleCancel = cancel
	s.pcMu.Unlock()
	if prev != nil {
		prev()
	}
}
