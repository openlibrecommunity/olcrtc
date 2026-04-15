// Package jazz implements the SaluteJazz WebRTC provider.
package jazz

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/provider"
	"github.com/pion/webrtc/v4"
)

// dcEngine implements mediaEngine on top of a pair of SCTP DataChannels labeled
// "_reliable". This is the transport Jazz has used since olcRTC started and is
// intentionally preserved bit-for-bit during the visual-transport refactor:
// any behavioural delta here would break shipping clients.
//
// Flow summary:
//
//   - publisher side: a DataChannel is created on pcPub up front (so that it
//     lands in the SDP offer); application bytes go through sendQueue and
//     processSendQueue drains them into DC.Send after wrapping in the Jazz
//     protobuf DataPacket envelope expected by the SFU.
//
//   - subscriber side: the SFU opens a mirrored DataChannel from its side,
//     which surfaces on pcSub.OnDataChannel; we read from it, strip the
//     DataPacket envelope, and forward payloads via onPayload.
//
// All DC-specific concerns — maximum message size, per-message delay, protobuf
// framing — live here so that signaling / reconnect code in peer.go does not
// need to know anything about DataChannels.
type dcEngine struct {
	dc *webrtc.DataChannel

	// onPayload is the upward data path callback set by Peer via
	// setOnPayload. It is invoked once a full application frame has been
	// reassembled (which for DC means "the remote peer called Send exactly
	// once"; DC preserves message boundaries so no reassembly is needed).
	onPayload func([]byte)

	// onClosed is invoked when the transport dies so Peer can kick a
	// reconnect. Guarded by a sync.Once indirection inside Peer.
	onClosed func()

	// sendQ carries outbound application payloads from Peer.Send to the
	// processSendQueue goroutine. The capacity (5000) matches the original
	// implementation so that back-pressure behaviour is unchanged.
	sendQ           chan []byte
	sendQueueClosed atomic.Bool

	// readyCh is closed once the publisher DC is open and we are ready to
	// transmit. Peer.Connect waits on this channel as part of its own
	// readiness handshake.
	readyCh     chan struct{}
	readyClosed atomic.Bool

	// closeCh stops processSendQueue. closeOnce guards it so repeat Close()
	// calls remain idempotent.
	closeCh   chan struct{}
	closeOnce sync.Once

	// wg tracks the processSendQueue goroutine for clean shutdown.
	wg sync.WaitGroup
}

// newDCEngine allocates a dcEngine with empty channels. No PeerConnection work
// is done here; that happens in setup(), which Peer calls from Connect().
func newDCEngine() *dcEngine {
	return &dcEngine{
		sendQ:   make(chan []byte, 5000),
		readyCh: make(chan struct{}),
		closeCh: make(chan struct{}),
	}
}

// setup implements mediaEngine.setup. It creates the publisher DataChannel
// (so SDP offer includes its m-section) and wires both publisher OnOpen/
// OnClose/OnMessage and the subscriber-side OnDataChannel handler.
//
// Returned channel is closed when the publisher DC fires OnOpen — that is the
// moment at which the engine is willing to drain sendQ.
func (e *dcEngine) setup(pcPub, pcSub *webrtc.PeerConnection) (<-chan struct{}, error) {
	ordered := true
	dc, err := pcPub.CreateDataChannel("_reliable", &webrtc.DataChannelInit{
		Ordered: &ordered,
	})
	if err != nil {
		return nil, fmt.Errorf("create datachannel: %w", err)
	}
	e.dc = dc

	// Publisher-side handlers. OnOpen is our "ready" edge; OnClose triggers
	// reconnect; OnMessage is the happy-path incoming data (SFU routes some
	// peers' traffic through the publisher DC as well as the subscriber one).
	dc.OnOpen(func() {
		logger.Verbosef("[Jazz] Publisher DC opened: %s", dc.Label())
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.processSendQueue()
		}()
		e.markReady()
	})

	dc.OnClose(func() {
		logger.Verbosef("[Jazz] Publisher DC closed")
		if e.onClosed != nil {
			e.onClosed()
		}
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		e.handleIncoming(msg.Data, "publisher")
	})

	// Subscriber-side: the SFU initiates a _reliable DC from its end; the
	// pion PC surfaces it through OnDataChannel. We only care about the
	// reliable channel and ignore any other label (the SFU occasionally
	// opens control channels we do not use).
	pcSub.OnDataChannel(func(remote *webrtc.DataChannel) {
		logger.Verbosef("[Jazz] Received subscriber DataChannel: %s", remote.Label())
		if remote.Label() != "_reliable" {
			return
		}
		remote.OnMessage(func(msg webrtc.DataChannelMessage) {
			e.handleIncoming(msg.Data, "subscriber")
		})
	})

	return e.readyCh, nil
}

// handleIncoming mirrors the original Peer.handleIncomingMessage: try to
// decode the Jazz protobuf DataPacket envelope, and if that fails fall back to
// treating the bytes as a raw payload. Either way the result goes upward
// through onPayload.
func (e *dcEngine) handleIncoming(data []byte, source string) {
	logger.Verbosef("[Jazz] Received %d bytes on %s DC (raw)", len(data), source)

	payload, ok := DecodeDataPacket(data)
	if !ok {
		// Backwards compatibility: some early peers emit bare bytes without
		// the protobuf envelope. Keeping this path identical to the pre-
		// refactor behaviour matters for mixed-version rooms.
		logger.Debugf("[Jazz] Failed to decode DataPacket, trying raw")
		if e.onPayload != nil && len(data) > 0 {
			e.onPayload(data)
		}
		return
	}

	logger.Verbosef("[Jazz] Decoded DataPacket: %d bytes payload", len(payload))
	if e.onPayload != nil && len(payload) > 0 {
		e.onPayload(payload)
	}
}

// markReady closes readyCh exactly once. Guarded by an atomic because OnOpen
// may be invoked multiple times if the transport is renegotiated.
func (e *dcEngine) markReady() {
	if e.readyClosed.CompareAndSwap(false, true) {
		close(e.readyCh)
	}
}

// send enqueues one payload for transmission. The contract matches the
// pre-refactor Peer.Send: return immediately on a healthy channel, return a
// provider error otherwise. The 50 ms enqueue timeout is preserved verbatim.
func (e *dcEngine) send(data []byte) error {
	if e.dc == nil || e.dc.ReadyState() != webrtc.DataChannelStateOpen {
		return provider.ErrDataChannelNotReady
	}
	if e.sendQueueClosed.Load() {
		return provider.ErrSendQueueClosed
	}

	select {
	case e.sendQ <- data:
		return nil
	case <-time.After(50 * time.Millisecond):
		return provider.ErrSendQueueTimeout
	}
}

// processSendQueue is the outbound drain loop. It is started from OnOpen and
// exits on close. Each payload is wrapped in the Jazz DataPacket envelope
// before hitting DC.Send so that the SFU forwards it as a "data" message type.
// The sendDelay sleep is inherited from the original code as SFU rate-limit
// insurance; do not drop it without measuring first.
func (e *dcEngine) processSendQueue() {
	for {
		select {
		case <-e.closeCh:
			return
		case data := <-e.sendQ:
			if len(data) > maxDataChannelMessageSize {
				logger.Debugf("[Jazz] Message too large: %d bytes (max %d)", len(data), maxDataChannelMessageSize)
				continue
			}

			encoded := EncodeDataPacket(data)
			logger.Verbosef("[Jazz] Sending %d bytes (encoded to %d bytes)", len(data), len(encoded))

			if err := e.dc.Send(encoded); err != nil {
				logger.Debugf("send error: %v", err)
				// Let Peer know so it can reconnect under the same policy
				// it uses for any other transport failure.
				if e.onClosed != nil {
					e.onClosed()
				}
				return
			}
			time.Sleep(sendDelay)
		}
	}
}

// canSend mirrors the pre-refactor logic in Peer.CanSend: healthy DC plus
// plenty of headroom in the queue.
func (e *dcEngine) canSend() bool {
	if e.dc == nil || e.dc.ReadyState() != webrtc.DataChannelStateOpen {
		return false
	}
	return len(e.sendQ) < 4000
}

// bufferedAmount returns DC-level buffering. Zero when the channel is gone.
func (e *dcEngine) bufferedAmount() uint64 {
	if e.dc != nil {
		return e.dc.BufferedAmount()
	}
	return 0
}

// sendQueue exposes the internal channel for the Provider interface surface.
// External callers today do not actually drain it, but we keep the hook to
// avoid breaking the wrapper in provider.go.
func (e *dcEngine) sendQueue() chan []byte {
	return e.sendQ
}

// close is idempotent: safe to call from Peer.Close as well as from an
// internal reconnect path. It stops processSendQueue, waits briefly for it to
// exit, and closes the underlying DC. Errors on dc.Close are logged through
// the caller; we swallow them here because the channel may already be gone.
func (e *dcEngine) close() error {
	e.sendQueueClosed.Store(true)

	e.closeOnce.Do(func() {
		close(e.closeCh)
	})

	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}

	if e.dc != nil {
		_ = e.dc.Close()
	}
	return nil
}

// setOnPayload installs the upward data callback. Called once by Peer.Connect.
func (e *dcEngine) setOnPayload(cb func(payload []byte)) { e.onPayload = cb }

// setOnClosed installs the transport-died callback. Called once by
// Peer.Connect. Peer debounces reconnect attempts itself so a single bool
// field is enough.
func (e *dcEngine) setOnClosed(cb func()) { e.onClosed = cb }
