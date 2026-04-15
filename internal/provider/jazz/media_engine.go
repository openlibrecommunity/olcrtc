// Package jazz implements the SaluteJazz WebRTC provider.
package jazz

import "github.com/pion/webrtc/v4"

// mediaEngine is the contract between the Jazz signaling core (Peer) and a
// concrete transport implementation that carries application bytes over a
// WebRTC session.
//
// The signaling layer — WebSocket handshake, SDP exchange, ICE negotiation,
// reconnect bookkeeping — lives in Peer and is identical for every transport.
// What differs between transports is *what media object* is installed on the
// PeerConnection pair and *how bytes cross it*:
//
//   - dcEngine installs a reliable SCTP DataChannel on the publisher PC and
//     listens for the mirrored one on the subscriber PC (current, shipping
//     behaviour).
//   - A future visual engine will instead add a VideoTrack to the publisher PC
//     and read an incoming VideoTrack from the subscriber via OnTrack, encoding
//     bytes as steganographic patterns inside the frames.
//
// All engines share the Peer's reconnect / lifecycle machinery: when a
// transport dies, the engine invokes the closed callback and Peer decides
// whether to reconnect. This keeps all the flaky WebSocket and ICE logic in
// exactly one place.
type mediaEngine interface {
	// setup installs the engine's media objects on the two PeerConnections
	// belonging to the Peer (subscriber pcSub receives remote media, publisher
	// pcPub sends local media). It must return a channel that becomes readable
	// when the engine is ready to accept send() calls — i.e. the underlying
	// transport is open. The channel should be closed once, never written to.
	//
	// setup is called exactly once from Peer.Connect, before the signaling
	// goroutine is started, so that SDP offer/answer generated later already
	// includes the engine's m-section.
	setup(pcPub, pcSub *webrtc.PeerConnection) (<-chan struct{}, error)

	// send hands a single application payload to the engine for transmission.
	// The engine is responsible for chunking, framing, flow control and any
	// transport-specific encoding (e.g. protobuf DataPacket for Jazz DC).
	// It must not block for longer than a few tens of milliseconds; if the
	// engine is saturated it should return an error and let the caller retry.
	send(data []byte) error

	// canSend reports whether the engine believes another send() call would
	// succeed without blocking or being dropped. Used by the client-side load
	// balancer to stagger peers.
	canSend() bool

	// bufferedAmount returns the number of bytes queued inside the transport
	// (e.g. DataChannel.BufferedAmount) so callers can back-pressure upstream.
	bufferedAmount() uint64

	// sendQueue exposes the engine's internal channel to external callers that
	// want to observe or drain it. Only present because the Provider interface
	// surfaces it; new engines are free to return a read-only placeholder.
	sendQueue() chan []byte

	// close tears down the engine's transport resources. It must be idempotent
	// and safe to call from any goroutine. Peer.Close calls it after WebSocket
	// teardown.
	close() error

	// setOnPayload wires the upward data path: whenever the engine decodes a
	// full application payload from the remote side, it invokes this callback
	// with the cleartext bytes. Called once from Peer.Connect before setup.
	setOnPayload(cb func(payload []byte))

	// setOnClosed wires the engine → Peer notification used when the transport
	// unexpectedly dies (DC closed, video track ended, ...). Peer uses this to
	// schedule reconnect under the same policy it uses for WebSocket drops.
	setOnClosed(cb func())
}

// trackMetadataProvider is an optional engine extension for transports that
// need an explicit Jazz-side `rtc:track:add` announcement before sending the
// publisher SDP offer.
type trackMetadataProvider interface {
	trackMetadata() map[string]any
}

// publisherTrackAttacher is an optional engine extension for transports that
// must delay attaching their local media to the publisher PC until Jazz has
// acknowledged rtc:track:add for that media.
type publisherTrackAttacher interface {
	attachPublisherTrack(pcPub *webrtc.PeerConnection) error
}

// publisherReadyNotifier is an optional engine hook for transports that are
// not actually ready to transmit until the publisher offer/answer exchange has
// completed.
type publisherReadyNotifier interface {
	markPublisherReady()
}

// publisherTrackSequencer is an optional engine extension for transports that
// need multiple sequential publish cycles before the transport is fully ready.
type publisherTrackSequencer interface {
	nextTrackMetadata() map[string]any
	onTrackPublished(cid, sid string)
	publisherReady() bool
}
