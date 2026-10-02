package jitsi

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/engine"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
)

const reconnectGrace = 20 * time.Second

var fallbackEpoch atomic.Uint32 //nolint:gochecknoglobals // crypto/rand fallback counter

func randomEpoch() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		v := fallbackEpoch.Add(1)
		if v == 0 {
			return fallbackEpoch.Add(1)
		}
		return v
	}
	v := binary.BigEndian.Uint32(b[:])
	if v == 0 {
		return 1
	}
	return v
}

func (s *Session) encodeBridgeFrame(data []byte, peerID string) ([]byte, error) {
	const epochHeaderLen = 8
	if len(data)+len(bridgeMagic)+epochHeaderLen > bridgeMaxMessageSize {
		return nil, ErrSendTooLarge
	}
	framed := make([]byte, len(bridgeMagic)+epochHeaderLen+len(data))
	copy(framed, bridgeMagic[:])
	off := len(bridgeMagic)
	binary.BigEndian.PutUint32(framed[off:off+4], s.localEpoch.Load())
	binary.BigEndian.PutUint32(framed[off+4:off+epochHeaderLen], s.peerEpochFor(peerID))
	copy(framed[off+epochHeaderLen:], data)
	return framed, nil
}

func (s *Session) peerEpochFor(peerID string) uint32 {
	if peerID == "" || s.onPeerData == nil {
		return s.peerEpoch.Load()
	}
	s.peerEpochMu.Lock()
	defer s.peerEpochMu.Unlock()
	return s.peerEpochs[peerID]
}

func (s *Session) outboundFrameCurrent(frame []byte) bool {
	const epochHeaderLen = 8
	if len(frame) < len(bridgeMagic)+epochHeaderLen {
		return false
	}
	off := len(bridgeMagic)
	return binary.BigEndian.Uint32(frame[off:off+4]) == s.localEpoch.Load()
}

type epochFrame struct {
	senderEpoch   uint32
	receiverEpoch uint32
	body          []byte
}

// parseEpochFrame validates the epoch header shared by broadcast and per-peer
// receive paths. It rejects local echoes and frames for an old local epoch.
func (s *Session) parseEpochFrame(payload []byte) (epochFrame, bool) {
	const epochHeaderLen = 8
	if len(payload) < len(bridgeMagic)+epochHeaderLen {
		return epochFrame{}, false
	}
	off := len(bridgeMagic)
	local := s.localEpoch.Load()
	frame := epochFrame{
		senderEpoch:   binary.BigEndian.Uint32(payload[off : off+4]),
		receiverEpoch: binary.BigEndian.Uint32(payload[off+4 : off+epochHeaderLen]),
		body:          payload[off+epochHeaderLen:],
	}
	if frame.senderEpoch == 0 || frame.senderEpoch == local {
		return epochFrame{}, false
	}
	if frame.receiverEpoch != 0 && frame.receiverEpoch != local {
		logger.Debugf("jitsi: drop stale bridge frame peerEpoch=0x%08x localEpoch=0x%08x",
			frame.receiverEpoch, local)
		return epochFrame{}, false
	}
	return frame, true
}

// maxPeerEpochs caps the per-endpoint epoch table. The endpoint name comes
// from the bridge message and is recorded before anything authenticates the
// sender, so without a cap any room participant can grow this map by naming a
// new sender on every frame. The table is only a routing hint; refusing to
// learn new names past the cap costs nothing beyond an unusually crowded room.
const maxPeerEpochs = 256

func (s *Session) acceptPeerEpochFrame(from string, payload []byte) ([]byte, bool) {
	frame, ok := s.parseEpochFrame(payload)
	if !ok {
		return nil, false
	}
	senderEpoch := frame.senderEpoch
	s.peerEpochMu.Lock()
	prev, known := s.peerEpochs[from]
	switch {
	case known:
		if prev != senderEpoch {
			s.peerEpochs[from] = senderEpoch
		}
	case len(s.peerEpochs) < maxPeerEpochs:
		s.peerEpochs[from] = senderEpoch
	}
	s.peerEpochMu.Unlock()
	return frame.body, true
}

func (s *Session) acceptEpochFrame(payload []byte) ([]byte, bool) {
	frame, ok := s.parseEpochFrame(payload)
	if !ok {
		return nil, false
	}
	senderEpoch, receiverEpoch := frame.senderEpoch, frame.receiverEpoch

	if s.requireTargetedPeer && s.onPeerData == nil {
		if receiverEpoch != s.localEpoch.Load() {
			logger.Debugf("jitsi: drop untargeted bridge frame senderEpoch=0x%08x localEpoch=0x%08x",
				senderEpoch, s.localEpoch.Load())
			return nil, false
		}
		if confirmed := s.peerEpoch.Load(); confirmed != 0 && senderEpoch != confirmed {
			logger.Debugf("jitsi: drop frame from unauthenticated peer senderEpoch=0x%08x peerEpoch=0x%08x",
				senderEpoch, confirmed)
			return nil, false
		}
		return frame.body, true
	}

	// A peer epoch change identifies fresh peer state, not a reason to
	// reconnect. Receiving the frame proves that our bridge is still alive.
	prev := s.peerEpoch.Load()
	if prev == 0 {
		s.peerEpoch.Store(senderEpoch)
	} else if prev != senderEpoch {
		s.peerEpoch.CompareAndSwap(prev, senderEpoch)
		if s.inReconnectGrace() {
			logger.Debugf("jitsi: peer epoch changed during grace period (0x%08x -> 0x%08x)",
				prev, senderEpoch)
		} else {
			logger.Debugf("jitsi: peer epoch changed (0x%08x -> 0x%08x), accepting fresh peer state",
				prev, senderEpoch)
		}
	}
	return frame.body, true
}

// LocalPeerID returns the local bridge epoch carried in routing frames.
func (s *Session) LocalPeerID() string {
	return fmt.Sprintf("%08x", s.localEpoch.Load())
}

// ConfirmPeer binds targeted single-peer traffic to an authenticated epoch.
func (s *Session) ConfirmPeer(peerID string) error {
	value, err := strconv.ParseUint(peerID, 16, 32)
	if err != nil {
		return fmt.Errorf("parse peer id %q: %w", peerID, err)
	}
	epoch := uint32(value)
	if epoch == 0 {
		return fmt.Errorf("%w: epoch 0x%08x", engine.ErrInvalidPeerID, epoch)
	}
	s.peerEpoch.Store(epoch)
	return nil
}

func (s *Session) inReconnectGrace() bool {
	last := s.lastReconnectAt.Load()
	if last == 0 {
		return false
	}
	return time.Since(time.Unix(0, last)) < reconnectGrace
}

// latchPeerEndpoint records the first valid sender and rebinds when that peer
// reconnects under a new JVB endpoint ID. Empty senders are broadcasts.
func (s *Session) latchPeerEndpoint(from string) {
	if from == "" {
		return
	}
	cur := s.peerEndpoint.Load()
	if cur == nil {
		s.peerEndpoint.CompareAndSwap(nil, &from)
		return
	}
	if *cur == from {
		return
	}
	newFrom := from
	if s.peerEndpoint.CompareAndSwap(cur, &newFrom) {
		logger.Debugf("jitsi: peer latch re-bound %s -> %s (peer reconnected)", *cur, from)
	}
}

func (s *Session) resetPeerEpochs() {
	s.peerEpochMu.Lock()
	clear(s.peerEpochs)
	s.peerEpochMu.Unlock()
}

// PeerSeen implements transport.PeerObserver: a peer epoch is latched, or
// the bridge lists another occupant of the room (the library already leaves
// out our own nick and the focus).
func (s *Session) PeerSeen() bool {
	if s.peerEpoch.Load() != 0 {
		return true
	}
	jSess := s.jSess.Load()
	return jSess != nil && len(jSess.Endpoints()) > 0
}

// WaitForPeer blocks until the encrypted handshake has confirmed a peer epoch.
func (s *Session) WaitForPeer(ctx context.Context) error {
	const pollInterval = 50 * time.Millisecond
	for {
		if s.peerEpoch.Load() != 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for peer: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
