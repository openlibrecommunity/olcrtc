package jitsi

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/zarazaex69/j"

	"github.com/openlibrecommunity/olcrtc/internal/logger"
)

const (
	// bridgeMaxMessageSize stays below JVB's practical 16 KiB websocket limit.
	bridgeMaxMessageSize = 16 * 1024
	bridgeOpenTimeout    = 30 * time.Second
	// sendLoop must not wait through a full reconnect because it is the only
	// consumer of both bounded queues. Old-epoch frames are stale anyway.
	jSessionWaitTimeout = 2 * time.Second

	// colibriClassEndpointMessage is the JVB bridge-channel message class
	// used for opaque raw payloads (see endpointMessage and decodeRaw).
	colibriClassEndpointMessage = "EndpointMessage"
)

var bridgeMagic = [4]byte{'O', 'L', 'R', '1'} //nolint:gochecknoglobals // wire protocol constant

type bridgeOutbound struct {
	to   string
	data []byte
}

func (s *Session) openBridgeWS(ctx context.Context, jSess *j.Session) error {
	return s.openBridge(ctx, jSess, "", "colibri-ws", jSess.OpenBridge)
}

func (s *Session) openBridgeSCTP(ctx context.Context, jSess *j.Session) error {
	return s.openBridge(ctx, jSess, " sctp", "sctp", jSess.WaitBridgeSCTP)
}

func (s *Session) openBridge(
	ctx context.Context,
	jSess *j.Session,
	errorSuffix string,
	transport string,
	open func(context.Context) error,
) error {
	bctx, bcancel := context.WithTimeout(ctx, bridgeOpenTimeout)
	err := open(bctx)
	bcancel()
	if err != nil {
		return fmt.Errorf("open bridge%s: %w", errorSuffix, err)
	}
	s.peerEndpoint.Store(nil)
	s.peerVideoSSRC.Store(0)
	s.markBridgeReady()
	logger.Infof("jitsi: bridge open %s (endpoints=%v)", transport, jSess.Endpoints())
	return nil
}

// Send queues a broadcast bridge frame without blocking.
func (s *Session) Send(data []byte) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	if !s.bridgeReady.Load() {
		return ErrBridgeNotReady
	}
	framed, err := s.encodeBridgeFrame(data, "")
	if err != nil {
		return err
	}
	return enqueueBridgeFrame(s, s.sendQueue, framed, framed)
}

// SendTo queues a bridge frame for a specific Jitsi endpoint.
func (s *Session) SendTo(peerID string, data []byte) error {
	if peerID == "" {
		return s.Send(data)
	}
	if s.closed.Load() {
		return ErrSessionClosed
	}
	if !s.bridgeReady.Load() {
		return ErrBridgeNotReady
	}
	framed, err := s.encodeBridgeFrame(data, peerID)
	if err != nil {
		return err
	}
	outbound := bridgeOutbound{to: peerID, data: framed}
	return enqueueBridgeFrame(s, s.peerSendQueue, framed, outbound)
}

func enqueueBridgeFrame[T any](s *Session, queue chan<- T, framed []byte, value T) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	if !s.bridgeReady.Load() {
		return ErrBridgeNotReady
	}
	if len(framed) > bridgeMaxMessageSize {
		return ErrSendTooLarge
	}
	select {
	case queue <- value:
		return nil
	case <-s.done:
		return ErrSessionClosed
	default:
		return ErrSendQueueFull
	}
}

func (s *Session) sendLoop() {
	for {
		select {
		case <-s.done:
			return
		case data, ok := <-s.sendQueue:
			if !ok {
				return
			}
			s.sendBridgeFrame("", data)
		case frame, ok := <-s.peerSendQueue:
			if !ok {
				return
			}
			s.sendBridgeFrame(frame.to, frame.data)
		}
	}
}

func (s *Session) sendBridgeFrame(to string, data []byte) {
	if !s.outboundFrameCurrent(data) {
		return
	}
	jSess := s.waitJSession()
	if jSess == nil {
		return
	}
	if !s.outboundFrameCurrent(data) {
		return
	}
	if err := sendEndpointRaw(jSess, to, data); err != nil {
		if s.closed.Load() {
			return
		}
		logger.Debugf("jitsi bridge send: %v", err)
	}
}

// endpointMessage mirrors the wire shape of a Jitsi Videobridge EndpointMessage.
// Field order matters: some Jackson versions on the bridge side drop the
// payload field entirely if it appears before "to" (see
// https://github.com/jitsi/jitsi-videobridge/pull/2424), so this is declared
// and marshalled as a struct (not a map) to force colibriClass, to,
// msgPayload in that exact order regardless of Go's map-key sorting.
type endpointMessage struct {
	ColibriClass string             `json:"colibriClass"` //nolint:tagliatelle // JVB wire protocol uses camelCase
	To           string             `json:"to"`           //nolint:tagliatelle // JVB wire protocol uses camelCase
	MsgPayload   endpointRawPayload `json:"msgPayload"`   //nolint:tagliatelle // JVB wire protocol uses camelCase
}

type endpointRawPayload struct {
	Raw string `json:"raw"`
}

// sendEndpointRaw sends opaque bytes as msgPayload.raw instead of the
// nonstandard top-level "raw" field used by the underlying j library's
// BridgeSendRaw. Per the JVB EndpointMessage docs, the payload belongs under
// msgPayload; some bridge builds silently drop the frame otherwise. See
// olcrtc#143.
func sendEndpointRaw(jSess *j.Session, to string, data []byte) error {
	br := jSess.Bridge()
	if br == nil {
		return ErrBridgeNotReady
	}
	msg := endpointMessage{
		ColibriClass: colibriClassEndpointMessage,
		To:           to,
		MsgPayload: endpointRawPayload{
			Raw: base64.StdEncoding.EncodeToString(data),
		},
	}
	if err := br.SendJSON(msg); err != nil {
		return fmt.Errorf("send endpoint message: %w", err)
	}
	return nil
}

// setJSession installs a session and republishes the readiness signal used by
// sendLoop. Passing nil rearms the signal for the next reconnect.
func (s *Session) setJSession(sess *j.Session) *j.Session {
	old := s.jSess.Swap(sess)

	s.jSessMu.Lock()
	defer s.jSessMu.Unlock()
	if s.jSessReady == nil {
		s.jSessReady = make(chan struct{})
	}
	if sess == nil {
		select {
		case <-s.jSessReady:
			s.jSessReady = make(chan struct{})
		default:
		}
		return old
	}
	select {
	case <-s.jSessReady:
	default:
		close(s.jSessReady)
	}
	return old
}

func (s *Session) waitJSession() *j.Session {
	if s.closed.Load() {
		return nil
	}
	if jSess := s.jSess.Load(); jSess != nil {
		return jSess
	}

	s.jSessMu.Lock()
	if s.jSessReady == nil {
		s.jSessReady = make(chan struct{})
	}
	ready := s.jSessReady
	s.jSessMu.Unlock()

	timer := time.NewTimer(jSessionWaitTimeout)
	defer timer.Stop()
	select {
	case <-ready:
		return s.jSess.Load()
	case <-s.done:
		return nil
	case <-timer.C:
		return nil
	}
}

// recvLoop consumes the bridge channel. Only one instance may run at a time:
// Connect, completeJingleSetup and finishReconnect each start one, and two
// loops racing on the same channel split frames between them and hand them to
// onData concurrently and out of order - which the record layer's replay
// window then rejects as junk. A later loop waits here until the previous one
// has seen its channel close.
func (s *Session) recvLoop() {
	s.recvMu.Lock()
	defer s.recvMu.Unlock()

	gen := s.bridgeGen.Load()
	jSess := s.jSess.Load()
	if jSess == nil || (s.onData == nil && s.onPeerData == nil) || !s.bridgeReady.Load() {
		logger.Debugf("jitsi: recvLoop early exit jSess=%v onData=%v onPeerData=%v bridgeReady=%v",
			jSess != nil, s.onData != nil, s.onPeerData != nil, s.bridgeReady.Load())
		return
	}
	msgs := jSess.BridgeMessages()
	if msgs == nil {
		logger.Debugf("jitsi: recvLoop: BridgeMessages() returned nil, exiting")
		return
	}
	logger.Debugf("jitsi: recvLoop started")
	for {
		select {
		case <-s.done:
			return
		case msg, ok := <-msgs:
			if !s.deliverBridgeMessageGen(gen, msg, ok) {
				return
			}
		}
	}
}

func (s *Session) deliverBridgeMessage(msg j.BridgeMessage, ok bool) bool {
	return s.deliverBridgeMessageGen(s.bridgeGen.Load(), msg, ok)
}

func (s *Session) deliverBridgeMessageGen(gen uint64, msg j.BridgeMessage, ok bool) bool {
	if !ok {
		if !s.closed.Load() {
			s.requestReconnectGen(gen, "jitsi bridge closed")
		}
		return false
	}
	payload, valid := bridgePayload(msg)
	if !valid {
		return true
	}
	if s.onPeerData != nil && msg.From != "" {
		return s.deliverPeerBridgePayload(msg.From, payload)
	}
	data, accepted := s.acceptEpochFrame(payload)
	if !accepted {
		return true
	}
	if !s.requireTargetedPeer || s.peerEpoch.Load() != 0 {
		s.latchPeerEndpoint(msg.From)
	}
	if len(data) == 0 {
		return true
	}
	s.onData(data)
	return true
}

func bridgePayload(msg j.BridgeMessage) ([]byte, bool) {
	payload := decodeRaw(msg)
	if payload == nil {
		return nil, false
	}
	if len(payload) < len(bridgeMagic) || !bytes.Equal(payload[:len(bridgeMagic)], bridgeMagic[:]) {
		return nil, false
	}
	return payload, true
}

func (s *Session) deliverPeerBridgePayload(from string, payload []byte) bool {
	data, ok := s.acceptPeerEpochFrame(from, payload)
	if !ok || len(data) == 0 {
		return true
	}
	s.onPeerData(from, data)
	return true
}

// decodeRaw extracts the base64 payload from an EndpointMessage. It accepts
// both the standard msgPayload.raw shape (as sent by sendEndpointRaw) and the
// legacy top-level "raw" field (as sent by older olcrtc builds, or by peers
// still on the j library's BridgeSendRaw), for backward compatibility. See
// olcrtc#143.
func decodeRaw(m j.BridgeMessage) []byte {
	if m.Class != colibriClassEndpointMessage {
		return nil
	}
	enc, ok := rawFieldFrom(m.Fields)
	if !ok {
		return nil
	}
	out, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil
	}
	return out
}

func rawFieldFrom(fields map[string]any) (string, bool) {
	if payload, ok := fields["msgPayload"].(map[string]any); ok {
		if raw, ok := payload["raw"].(string); ok {
			return raw, true
		}
	}
	raw, ok := fields["raw"].(string)
	return raw, ok
}

func (s *Session) markBridgeReady() {
	s.bridgeGen.Add(1)
	s.bridgeReady.Store(true)
}
