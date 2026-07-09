package multipath

import "sync"

// PathManager decides how many carrier paths a Bond should be maintaining
// and observes Bond's live topology as paths come and go. Phase 1 only ships
// ManualPathManager, a passive bookkeeper: the caller decides path count by
// calling Bond.AddPath directly. A future "smart" manager (bandwidth
// probing, autoscaling call count) can implement this interface and plug
// into Bond without touching the aggregation/scheduling logic.
type PathManager interface {
	// DesiredPaths reports how many paths the manager currently wants alive.
	DesiredPaths() int
	// OnPathAdded notifies the manager that pathIndex joined the bond.
	OnPathAdded(pathIndex uint16)
	// OnPathRemoved notifies the manager that pathIndex died.
	OnPathRemoved(pathIndex uint16)
}

// ManualPathManager is the Phase 1 PathManager: path count is whatever the
// caller adds/removes explicitly. It only tracks the count for reporting.
type ManualPathManager struct {
	mu    sync.Mutex
	count int
}

// NewManualPathManager creates a manager that starts by expecting initial
// paths (informational only - it never rejects AddPath/OnPathRemoved calls).
func NewManualPathManager(initial int) *ManualPathManager {
	if initial < 0 {
		initial = 0
	}
	return &ManualPathManager{count: initial}
}

// DesiredPaths returns the number of paths currently registered.
func (m *ManualPathManager) DesiredPaths() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

// OnPathAdded increments the tracked path count.
func (m *ManualPathManager) OnPathAdded(uint16) {
	m.mu.Lock()
	m.count++
	m.mu.Unlock()
}

// OnPathRemoved decrements the tracked path count, floored at zero.
func (m *ManualPathManager) OnPathRemoved(uint16) {
	m.mu.Lock()
	if m.count > 0 {
		m.count--
	}
	m.mu.Unlock()
}
