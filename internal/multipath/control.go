package multipath

import (
	"errors"

	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
)

// ErrNoControlPath is returned by the control-plane send when no control-capable
// path is currently alive.
var ErrNoControlPath = errors.New("multipath: no alive control path")

// controlBond wraps a *Bond to expose transport.ControlPlane. Only bonds whose
// paths all carry an isolated control plane are handed out as a *controlBond
// (see AsCarrier), so muxconn.NewControl builds a dedicated control smux session
// only when the underlying carriers can actually route control traffic
// out-of-band. A bare *Bond deliberately does NOT implement ControlPlane, which
// keeps inline control (handshake+liveness sharing the bulk smux session) for
// plain transports exactly as before Phase 5a - the type assertion
// bond.(transport.ControlPlane) then fails for those, and muxconn.NewControl
// returns nil.
type controlBond struct {
	*Bond
}

var (
	_ transport.Transport    = (*controlBond)(nil)
	_ transport.ControlPlane = (*controlBond)(nil)
)

// ControlSend routes a control frame over one live control-capable path.
func (cb *controlBond) ControlSend(data []byte) error { return cb.Bond.controlSend(data) }

// SetControlOnData installs cb as the receive sink for control frames arriving
// on any control-capable path.
func (cb *controlBond) SetControlOnData(fn func([]byte)) { cb.Bond.setControlOnData(fn) }

// ControlCanSend reports whether at least one control-capable path is ready.
func (cb *controlBond) ControlCanSend() bool { return cb.Bond.controlCanSend() }

// controlKind classifies a bond by how many of its registered paths carry an
// isolated control plane.
type controlKind int

const (
	// controlNone: no path carries a control plane (or the bond is empty).
	controlNone controlKind = iota
	// controlAll: every registered path carries a control plane.
	controlAll
	// controlMixed: some paths carry a control plane and some do not.
	controlMixed
)

// controlKind reports whether all / none / a mix of the bond's paths are
// control-capable. Paths in a bond are expected to be homogeneous (one
// transport type from one config), so controlAll/controlNone are the normal
// outcomes; controlMixed only happens with an inconsistent config.
func (b *Bond) controlKind() controlKind {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.paths) == 0 {
		return controlNone
	}
	ctrl := 0
	for _, p := range b.paths {
		if p.control != nil {
			ctrl++
		}
	}
	switch {
	case ctrl == 0:
		return controlNone
	case ctrl == len(b.paths):
		return controlAll
	default:
		return controlMixed
	}
}

// AsCarrier returns b as the transport.Transport that upper layers
// (muxconn/smux) should drive. When every registered path exposes an isolated
// control plane it returns a *controlBond, so muxconn.NewControl builds a
// dedicated control session over the bond; otherwise it returns the bare *Bond
// and control traffic stays inline on the bulk session. A mixed bond falls back
// to inline (bare *Bond) with a warning rather than pretending a control plane
// exists on paths that lack one.
//
// Call this after the bond's paths have been added: on the client all paths are
// present before dial; on the server the first path is added before the session
// is brought up, and paths are homogeneous so later joins do not change the
// classification.
func AsCarrier(b *Bond) transport.Transport {
	switch b.controlKind() {
	case controlAll:
		return &controlBond{Bond: b}
	case controlMixed:
		logger.Warnf("multipath: bond %x has a mix of control-capable and plain paths; "+
			"falling back to inline control", b.id)
		return b
	default:
		return b
	}
}

// setControlOnData stores fn and installs it on every control-capable path,
// present and future. All paths share the same callback, so a control frame
// received on any path is delivered up exactly once, transparently aggregating
// the receive side across paths. AddPath re-applies fn to paths that join
// later.
func (b *Bond) setControlOnData(fn func([]byte)) {
	b.controlMu.Lock()
	b.controlOnData = fn
	b.controlMu.Unlock()

	b.mu.RLock()
	paths := b.orderedPathsLocked()
	b.mu.RUnlock()
	for _, p := range paths {
		if p.control != nil {
			p.control.SetControlOnData(fn)
		}
	}
}

// controlCanSend reports whether any control-capable path is currently usable.
func (b *Bond) controlCanSend() bool {
	b.mu.RLock()
	paths := b.orderedPathsLocked()
	b.mu.RUnlock()
	for _, p := range paths {
		if p.controlAlive() {
			return true
		}
	}
	return false
}

// controlSend ships one control frame over a single live control-capable path,
// preferring the currently-selected sticky path so the ordered control stream
// stays on one carrier. On send failure it clears the selection and retries on
// another path (failover), bounded by the number of paths so a run of
// simultaneously-dying paths cannot spin forever.
func (b *Bond) controlSend(data []byte) error {
	b.mu.RLock()
	attempts := len(b.paths)
	b.mu.RUnlock()
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		p := b.pickControlPath()
		if p == nil {
			break
		}
		if err := p.control.ControlSend(data); err != nil {
			lastErr = err
			b.clearControlSelection(p.index)
			continue
		}
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return ErrNoControlPath
}

// pickControlPath returns the path the control stream should ride. It reuses
// the sticky selection while that path stays control-alive, and otherwise picks
// the lowest-index control-alive path and makes it the new sticky selection.
// Returns nil when no control-capable path is alive.
//
// Lock ordering: controlMu is taken first, then b.mu - never the reverse.
func (b *Bond) pickControlPath() *path {
	b.controlMu.Lock()
	defer b.controlMu.Unlock()

	if b.controlHasSel {
		b.mu.RLock()
		p := b.paths[b.controlSelected]
		b.mu.RUnlock()
		if p != nil && p.controlAlive() {
			return p
		}
		b.controlHasSel = false
	}

	b.mu.RLock()
	paths := b.orderedPathsLocked()
	b.mu.RUnlock()
	for _, p := range paths {
		if p.controlAlive() {
			b.controlSelected = p.index
			b.controlHasSel = true
			return p
		}
	}
	return nil
}

// clearControlSelection drops the sticky control selection if it still points
// at idx, forcing the next controlSend to re-select. Called after a send on idx
// fails so failover moves off the broken path.
func (b *Bond) clearControlSelection(idx uint16) {
	b.controlMu.Lock()
	if b.controlHasSel && b.controlSelected == idx {
		b.controlHasSel = false
	}
	b.controlMu.Unlock()
}
