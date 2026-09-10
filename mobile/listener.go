package mobile

// SessionListener receives tunnel session events from a Runtime.
//
// It is the mobile counterpart of the desktop client's "session <id> opened"
// log line: the moment a session is established in a room. A gomobile host
// cannot read the runtime's logs, so the event is delivered as a call instead,
// and it names the room - which is what lets the host tell a reconnect within
// the room it was in from a failover to another one, and act on the latter
// (refresh its room list through the new session, for instance).
type SessionListener interface {
	// OnSessionOpened reports one established session: on the initial
	// connect, after a reconnect within the same room, and after a failover.
	// room is the room the session is in, as given to SetRoom or
	// AddFailoverRoom; sessionID is the server-assigned id. It is called on
	// the runtime's connect path: return promptly and do the work elsewhere.
	OnSessionOpened(room string, sessionID string)
}

// SetSessionListener installs the listener for this Runtime, replacing any
// previous one; nil removes it. Safe to call while a generation is live - the
// next event goes to the new listener.
func (r *Runtime) SetSessionListener(l SessionListener) {
	r.mu.Lock()
	r.listener = l
	r.mu.Unlock()
}

// notifySessionOpened forwards a session event to the listener, unless the
// generation it came from is already on its way out: a session that opens
// while its generation is being stopped is not one the host should act on.
func (r *Runtime) notifySessionOpened(gen *runGeneration, room, sessionID string) {
	r.mu.Lock()
	listener := r.listener
	live := r.isCurrentGenerationLocked(gen) && !gen.stopRequested
	r.mu.Unlock()
	if listener == nil || !live {
		return
	}
	listener.OnSessionOpened(room, sessionID)
}
