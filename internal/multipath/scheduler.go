package multipath

import "sync/atomic"

// Scheduler selects which path an outgoing DATA frame is sent on. Bond calls
// Pick with a deterministically-ordered snapshot of its current paths (by
// path index) and expects back the index into that slice, or -1 if none of
// them are usable.
//
// The signature is intentionally internal-only (the path type is
// unexported) - Phase 1 ships a single built-in implementation. Future
// phases that want pluggable scheduling (e.g. weighted by measured
// bandwidth) can add exported knobs without changing Bond's public API.
type Scheduler interface {
	Pick(paths []*path) int
}

// roundRobinScheduler is a weighted round-robin scheduler. Phase 1 only
// implements equal weights, which degenerates to plain round-robin across
// whichever paths are currently alive.
type roundRobinScheduler struct {
	next atomic.Uint64
}

// NewRoundRobinScheduler returns the default Scheduler: even striping across
// all currently alive paths.
func NewRoundRobinScheduler() Scheduler {
	return &roundRobinScheduler{}
}

// Pick returns the next alive path in round-robin order, starting from
// wherever the internal cursor last left off so successive calls fan out
// across paths instead of favouring index 0.
func (s *roundRobinScheduler) Pick(paths []*path) int {
	n := len(paths)
	if n == 0 {
		return -1
	}
	cursor := s.next.Add(1) - 1
	start := int(cursor % uint64(n)) //nolint:gosec // n is a small slice length, no overflow risk
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if paths[idx].isAlive() {
			return idx
		}
	}
	return -1
}
