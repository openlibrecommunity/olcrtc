// Per-peer key pinning for multi-key rooms.
//
// A server serving several keys cannot know which key a peer holds until the
// peer's first record arrives. A PinGroup carries the candidate ring and the
// pin decision, shared by every Conn that belongs to the same peer (data and
// control planes), so whichever conn sees the first record that authenticates
// pins the key for all of them.
//
// Writes must never guess: sealing an early server frame (smux keepalive,
// window update) under the wrong key would make the peer drop it silently
// and corrupt the smux byte stream. Conn.Write therefore blocks until the
// group is pinned (or the conn closes). Single-key paths use PrePinned
// groups whose pin exists from birth, which makes the wait free and keeps
// the single-key behaviour exact.
//
// ai-generated: per-peer key pin over a crypto.KeyRing.
package muxconn

import (
	"sync"
	"sync/atomic"

	"github.com/openlibrecommunity/olcrtc/internal/crypto"
)

// PinGroup is the shared pairing state for one peer (or one single-link
// session generation). Create a fresh group per peer per (re)install - a
// reconnecting peer may return with a different key.
type PinGroup struct {
	ring    *crypto.KeyRing
	pinned  atomic.Pointer[crypto.RingEntry]
	pinOnce sync.Once
	pinCh   chan struct{}
}

// NewPinGroup builds an unpinned group over ring. The first record any member
// conn opens under a ring entry pins that entry.
func NewPinGroup(ring *crypto.KeyRing) *PinGroup {
	return &PinGroup{ring: ring, pinCh: make(chan struct{})}
}

// PrePinned builds a group that is pinned from birth - the exact single-key
// behaviour. keyID may be empty when the caller has no metering identity
// (client side, tests).
func PrePinned(keys *crypto.KeySet, keyID string) *PinGroup {
	g := NewPinGroup(crypto.SingleEntry(keys, keyID))
	g.pin(&g.ring.Entries()[0])
	return g
}

// pin publishes entry as the group's key. The first caller wins; the store
// happens before the channel close so a Write woken by pinCh always observes
// a non-nil pin.
func (g *PinGroup) pin(entry *crypto.RingEntry) {
	g.pinOnce.Do(func() {
		g.pinned.Store(entry)
		close(g.pinCh)
	})
}

// Pinned returns the pinned entry, or nil while pairing is still open.
func (g *PinGroup) Pinned() *crypto.RingEntry {
	return g.pinned.Load()
}

// KeyID returns the pinned key's metering id: "" while unpinned, or for a
// PrePinned group built without one.
func (g *PinGroup) KeyID() string {
	if e := g.pinned.Load(); e != nil {
		return e.KeyID
	}
	return ""
}

// PinWait returns the channel that closes once the group is pinned.
func (g *PinGroup) PinWait() <-chan struct{} { return g.pinCh }
