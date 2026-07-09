package multipath

import (
	"sync"

	"github.com/openlibrecommunity/olcrtc/internal/transport"
)

// path wraps one real transport.Transport carrier with the liveness state
// Bond needs to schedule sends and reroute on death. It is not exported -
// external packages only ever see Bond, which implements transport.Transport
// itself.
type path struct {
	index uint16
	tr    transport.Transport
	// control is tr type-asserted to transport.ControlPlane, or nil when the
	// underlying carrier has no isolated control plane. When non-nil the path
	// can carry the bond's control/handshake stream out-of-band of the striped
	// data plane (see control.go). It is left nil for peer paths, which route
	// control through peerCtl instead.
	control transport.ControlPlane

	// peer marks a path whose carrier is addressed by peerID (Phase 5b
	// peer-routing). For such a path Send/ControlSend target a specific remote
	// endpoint on a carrier that is SHARED across bonds and owned by the server:
	// the bond never subscribes to (or closes) the carrier itself - the server
	// demultiplexes inbound (peerID,data)/(peerID,control) and feeds the right
	// path via PathOnData/ControlIn.
	peer    bool
	peerTr  transport.PeerTransport
	peerCtl transport.PeerControlPlane
	peerID  string

	mu    sync.RWMutex
	alive bool
}

// newPath wraps tr as bond path pathIndex. Paths start alive optimistically;
// the first failed Connect/Send flips them dead. If tr exposes an isolated
// control plane (transport.ControlPlane) it is recorded so the bond can route
// control traffic over it.
func newPath(pathIndex uint16, tr transport.Transport) *path {
	control, _ := tr.(transport.ControlPlane)
	return &path{index: pathIndex, tr: tr, control: control, alive: true}
}

// newPeerPath wraps a peer-routing carrier pt addressed by peerID as bond path
// pathIndex. The carrier is shared and server-owned; per-peer control routes
// through pt's PeerControlPlane when it implements one.
func newPeerPath(pathIndex uint16, pt transport.PeerTransport, peerID string) *path {
	peerCtl, _ := pt.(transport.PeerControlPlane)
	return &path{
		index:   pathIndex,
		tr:      pt,
		peer:    true,
		peerTr:  pt,
		peerCtl: peerCtl,
		peerID:  peerID,
		alive:   true,
	}
}

// hasControl reports whether this path can carry the bond's isolated control
// stream at all (a singleton control plane for a plain path, or a per-peer
// control plane for a peer path).
func (p *path) hasControl() bool {
	return p.control != nil || p.peerCtl != nil
}

// isAlive reports whether the path is currently usable for scheduling. It
// consults both our own liveness flag (set by SetEndedCallback/failed sends)
// and the transport's own CanSend, since a transport can report congestion
// or a not-yet-ready state without ever calling its ended callback.
func (p *path) isAlive() bool {
	p.mu.RLock()
	alive := p.alive
	p.mu.RUnlock()
	return alive && p.tr.CanSend()
}

// markAlive flips the path back to alive, e.g. after a reconnect callback.
func (p *path) markAlive() {
	p.mu.Lock()
	p.alive = true
	p.mu.Unlock()
}

// markDead flips the path to dead, e.g. after its ended callback fires or a
// Send on it fails outright.
func (p *path) markDead() {
	p.mu.Lock()
	p.alive = false
	p.mu.Unlock()
}

// send writes frame to the underlying transport. Peer paths address their
// specific remote endpoint (SendTo); plain paths broadcast on the carrier (Send).
func (p *path) send(frame []byte) error {
	if p.peer {
		return p.peerTr.SendTo(p.peerID, frame)
	}
	return p.tr.Send(frame)
}

// sendControl writes a control frame over this path's control plane. Peer paths
// use the per-peer control plane (ControlSendTo); plain paths use the singleton
// control plane (ControlSend).
func (p *path) sendControl(frame []byte) error {
	if p.peer {
		if p.peerCtl == nil {
			return ErrNoControlPath
		}
		return p.peerCtl.ControlSendTo(p.peerID, frame)
	}
	if p.control == nil {
		return ErrNoControlPath
	}
	return p.control.ControlSend(frame)
}

// closeTransport closes the underlying carrier for a plain path. Peer paths
// share a server-owned carrier across bonds, so a bond must never close it -
// the server closes each carrier exactly once on shutdown.
func (p *path) closeTransport() error {
	if p.peer {
		return nil
	}
	return p.tr.Close()
}

// controlAlive reports whether this path can currently carry control traffic:
// it must have an isolated control plane, be marked alive by the bond, and its
// control plane must itself report ready-to-send.
func (p *path) controlAlive() bool {
	p.mu.RLock()
	alive := p.alive
	p.mu.RUnlock()
	if !alive {
		return false
	}
	if p.peer {
		return p.peerCtl != nil && p.peerCtl.ControlPeerCanSend(p.peerID)
	}
	return p.control != nil && p.control.ControlCanSend()
}
