package vp8channel

import (
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/logger"
)

// handleIncomingFrame parses the epoch header and delivers the KCP payload to
// the plane it belongs to.
func (p *streamTransport) handleIncomingFrame(frame []byte) {
	frameToken, src, dst, ok := parseEpochHeader(frame)
	if !ok {
		logger.Debugf("vp8channel: incoming frame bad header len=%d", len(frame))
		return
	}
	if frameToken != p.bindingToken {
		logger.Debugf("vp8channel: incoming frame token mismatch got=0x%08x want=0x%08x", frameToken, p.bindingToken)
		return
	}
	kcpPayload := frame[epochHdrLen:]
	if src == p.localEpochValue() || src == (p.localEpochValue()|controlEpochFlag) {
		return // own loopback (data or control)
	}
	p.peerSeen.Store(true)
	// Drop frames addressed to a different participant. dst==0 broadcasts are
	// always accepted (bootstrap before the sender learns our epoch).
	if !p.acceptsDst(dst) {
		return
	}

	// Control-plane frames have the high bit set in the src epoch field.
	// Route them to the control plane and never mix them with bulk data.
	if src&controlEpochFlag != 0 {
		p.handleControlFrame(src, dst, kcpPayload)
		return
	}

	if p.serverMode {
		p.handlePeerFrame(src, kcpPayload)
		return
	}

	p.handleSinglePeerData(src, kcpPayload)
}

// acceptsDst reports whether a frame addressed to dst is for us. dst==0 is a
// broadcast (accepted by everyone, used before the sender has learned our
// epoch). Otherwise the frame must target either our data epoch or our
// control epoch (data|controlEpochFlag).
func (p *streamTransport) acceptsDst(dst uint32) bool {
	if dst == 0 {
		return true
	}
	le := p.localEpochValue()
	return dst == le || dst == (le|controlEpochFlag)
}

// handleSinglePeerData delivers only frames from the peer authenticated by the
// encrypted handshake. Broadcast traffic cannot establish this binding.
func (p *streamTransport) handleSinglePeerData(src uint32, kcpPayload []byte) {
	switch {
	case !p.peerConfirmed.Load():
		return
	case src != p.peerEpoch.Load():
		p.maybePeerRestart(src)
		return
	default:
		p.lastPeerFrameNano.Store(time.Now().UnixNano())
	}

	if len(kcpPayload) == 0 {
		return
	}
	deliverKCPPayload(p.data.get(), kcpPayload)
}

// maybePeerRestart reads a frame from a non-latched epoch as a possible server
// restart and rebuilds the provider, at most once per restart. Both guards
// below are load-bearing - see "Peer restart detection" in the package doc for
// why silence alone is not evidence and what the rebuild path has to be.
func (p *streamTransport) maybePeerRestart(src uint32) {
	if !p.linkUnhealthy.Load() {
		return // no corroboration from our own control plane: room churn
	}
	if p.peerRestartGrace <= 0 {
		return
	}
	last := p.lastPeerFrameNano.Load()
	if last == 0 || time.Since(time.Unix(0, last)) < p.peerRestartGrace {
		return
	}
	if !p.peerRestarting.CompareAndSwap(false, true) {
		return // a rebuild is already in flight
	}
	logger.Infof("vp8channel: peer restart detected old=0x%08x new=0x%08x - rebuilding provider",
		p.peerEpoch.Load(), src)
	go p.stream.Reconnect("peer restart")
}

// handlePeerFrame routes incoming KCP data to a per-peer KCP runtime,
// creating one on demand. Each peer epoch gets its own independent KCP
// session so multiple clients can coexist in the same room.
func (p *streamTransport) handlePeerFrame(peerEpoch uint32, kcpPayload []byte) {
	// Registering the peer even for an empty keepalive is what refreshes its
	// idle timer, so a quiet-but-live client is not swept away.
	sess := p.peerSessionFor(peerEpoch)
	if sess == nil || len(kcpPayload) == 0 {
		return
	}

	deliverKCPPayload(sess.data, kcpPayload)
}
