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
	// data plane (see control.go).
	control transport.ControlPlane

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

// send writes frame to the underlying transport.
func (p *path) send(frame []byte) error {
	return p.tr.Send(frame)
}

// controlAlive reports whether this path can currently carry control traffic:
// it must have an isolated control plane, be marked alive by the bond, and its
// control plane must itself report ready-to-send.
func (p *path) controlAlive() bool {
	if p.control == nil {
		return false
	}
	p.mu.RLock()
	alive := p.alive
	p.mu.RUnlock()
	return alive && p.control.ControlCanSend()
}
