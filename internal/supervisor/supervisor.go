// Package supervisor runs ordered session profiles with failover.
//
// The profile list can be static (Config.Profiles) or dynamic: when
// Config.Reload is set it is consulted at every failover advance, so rooms
// added to the config while a session was live are picked up the instant that
// session ends - without restarting the process and without disturbing the
// active session (the reload only happens after the current profile exits).
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/app/session"
)

// DefaultRetryDelay is used between profile attempts when Config.RetryDelay is unset.
const DefaultRetryDelay = 2 * time.Second

// DefaultHistoryLimit bounds emitted status history when Config.HistoryLimit is unset.
const DefaultHistoryLimit = 20

const (
	// EventProfileStart marks a profile attempt starting.
	EventProfileStart = "profile_start"
	// EventProfileEnd marks a profile attempt ending.
	EventProfileEnd = "profile_end"
)

var (
	// ErrNoProfiles is returned when the supervisor is started without profiles.
	ErrNoProfiles = errors.New("supervisor: no profiles configured")
	// ErrMaxCyclesExceeded is returned after MaxCycles complete profile-list passes.
	ErrMaxCyclesExceeded = errors.New("supervisor: max failover cycles exceeded")
	errProfileCleanEnd   = errors.New("profile ended")
)

// Profile is one runnable session configuration in an ordered failover list.
type Profile struct {
	Name   string
	Config session.Config
}

// ProfileStatus summarizes one profile's failover history.
type ProfileStatus struct {
	Name        string
	Starts      int
	Failures    int
	CleanEnds   int
	LastStarted time.Time
	LastEnded   time.Time
	LastError   string
}

// Event is one bounded failover history entry.
type Event struct {
	Time    time.Time
	Type    string
	Profile string
	Cycle   int
	Error   string
}

// Status is a point-in-time view of the supervisor.
type Status struct {
	Cycle              int
	ActiveProfile      string
	ActiveProfileIndex int
	Profiles           []ProfileStatus
	History            []Event
	LastError          string
}

// Runner starts one session profile and blocks until it ends or fails.
type Runner func(ctx context.Context, cfg session.Config) error

// Config controls ordered failover behavior.
type Config struct {
	// Profiles is the initial ordered list. When Reload is nil it is the only list.
	Profiles []Profile

	// Reload, when set, returns the CURRENT ordered profile list. It is called at
	// every failover advance (never during a live session), so a room added to
	// the config on disk while a session was running is used the instant that
	// session ends. A nil, empty, or errored result keeps the last known list.
	Reload func() ([]Profile, error)

	RetryDelay time.Duration
	MaxCycles  int

	OnProfileStart func(profile Profile, cycle int)
	OnProfileEnd   func(profile Profile, cycle int, err error)
	OnStatus       func(status Status)
	HistoryLimit   int
}

// Run starts profiles in order. When a profile exits while ctx is still active,
// the supervisor waits RetryDelay, re-reads the list (if Reload is set), and
// advances to the profile AFTER the one that just ran (by name, wrapping to the
// top). Advancing by name keeps the client following a rolling window: once the
// room it just left is gone from the list, it lands on the new head.
//
// MaxCycles counts completed passes over the profiles on offer, judged against
// the list as reloaded after each profile ends. A list that grows while a
// profile is running therefore extends the current pass rather than waiting for
// the next one - which is what lets a host hand a running client its next room
// with MaxCycles set to 1, i.e. "try everything I have been given, once".
func Run(ctx context.Context, cfg Config, run Runner) error {
	// A negative delay parses fine from YAML and waitRetryDelay treats it as "no
	// wait", which turns failover into a busy loop against a profile that fails
	// immediately.
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = DefaultRetryDelay
	}

	current := append([]Profile(nil), cfg.Profiles...)
	refresh := func() []Profile {
		if cfg.Reload != nil {
			if next, err := cfg.Reload(); err == nil && len(next) > 0 {
				current = next
			}
		}
		return current
	}

	if len(refresh()) == 0 {
		return ErrNoProfiles
	}

	state := newStatusTracker(cfg.HistoryLimit, cfg.OnStatus)

	lastName := ""
	cycle := 1
	// Profiles started in the current cycle, by name. A cycle is one pass over
	// every profile on offer, and MaxCycles is measured against that - so a
	// list that grows while a profile is running extends the pass, instead of
	// the new entries being left for a cycle that never comes.
	startedThisCycle := map[string]bool{}
	for {
		if ctx.Err() != nil {
			return nil //nolint:nilerr // context cancellation is normal supervisor shutdown
		}

		profiles := refresh()
		if len(profiles) == 0 {
			if err := waitRetryDelay(ctx, cfg.RetryDelay); err != nil {
				return nil //nolint:nilerr // context cancellation during retry delay is normal shutdown
			}

			continue
		}

		idx := indexAfter(profiles, lastName)
		if idx == 0 && lastName != "" {
			cycle++
			startedThisCycle = map[string]bool{}
		}

		profile := profiles[idx]
		lastName = profile.Name
		startedThisCycle[profile.Name] = true

		state.start(profile.Name, cycle, idx)
		if cfg.OnProfileStart != nil {
			cfg.OnProfileStart(profile, cycle)
		}

		err := run(ctx, profile.Config)
		if ctx.Err() != nil {
			return nil //nolint:nilerr // context cancellation is normal supervisor shutdown
		}

		resultErr := profileResultError(profile.Name, err)
		state.end(profile.Name, cycle, err)
		if cfg.OnProfileEnd != nil {
			cfg.OnProfileEnd(profile, cycle, err)
		}

		if cfg.MaxCycles > 0 && cycle >= cfg.MaxCycles {
			// Judge against the list as it is NOW, not as it was when this
			// profile started: an entry added while it ran is still part of
			// this pass. The pass is over only when the next step would revisit
			// a profile already started in this cycle. Checked before the retry
			// delay, so a list with nowhere left to go fails fast.
			next := refresh()
			if len(next) > 0 && startedThisCycle[next[indexAfter(next, lastName)].Name] {
				return fmt.Errorf("%w after %d cycle(s): %w", ErrMaxCyclesExceeded, cycle, resultErr)
			}
		}

		if err := waitRetryDelay(ctx, cfg.RetryDelay); err != nil {
			return nil //nolint:nilerr // context cancellation during retry delay is normal shutdown
		}
	}
}

// indexAfter returns the position of the profile to run next: the one after the
// profile named lastName, wrapping to 0 at the end. If lastName is not in the
// list (first run, or it was dropped on reload) it returns 0.
func indexAfter(profiles []Profile, lastName string) int {
	if lastName == "" {
		return 0
	}

	for i, profile := range profiles {
		if profile.Name == lastName {
			return (i + 1) % len(profiles)
		}
	}

	return 0
}

func profileResultError(name string, err error) error {
	if err != nil {
		return fmt.Errorf("profile %q: %w", name, err)
	}

	return fmt.Errorf("profile %q: %w", name, errProfileCleanEnd)
}

type statusTracker struct {
	status       Status
	byName       map[string]*ProfileStatus
	notify       func(Status)
	historyLimit int
}

func newStatusTracker(historyLimit int, notify func(Status)) *statusTracker {
	if historyLimit == 0 {
		historyLimit = DefaultHistoryLimit
	}

	return &statusTracker{
		status:       Status{ActiveProfileIndex: -1},
		byName:       make(map[string]*ProfileStatus),
		notify:       notify,
		historyLimit: historyLimit,
	}
}

func (t *statusTracker) profile(name string) *ProfileStatus {
	profile := t.byName[name]
	if profile == nil {
		profile = &ProfileStatus{Name: name}
		t.byName[name] = profile
	}

	return profile
}

func (t *statusTracker) start(name string, cycle, idx int) {
	now := time.Now()
	profile := t.profile(name)
	profile.Starts++
	profile.LastStarted = now
	t.status.Cycle = cycle
	t.status.ActiveProfile = name
	t.status.ActiveProfileIndex = idx
	t.appendHistory(Event{
		Time:    now,
		Type:    EventProfileStart,
		Profile: name,
		Cycle:   cycle,
	})
	t.emit()
}

func (t *statusTracker) end(name string, cycle int, err error) {
	now := time.Now()
	profile := t.profile(name)
	profile.LastEnded = now
	event := Event{
		Time:    now,
		Type:    EventProfileEnd,
		Profile: name,
		Cycle:   cycle,
	}

	if err != nil {
		profile.Failures++
		profile.LastError = err.Error()
		t.status.LastError = fmt.Sprintf("profile %q: %v", name, err)
		event.Error = err.Error()
	} else {
		profile.CleanEnds++
		profile.LastError = ""
		t.status.LastError = fmt.Sprintf("profile %q ended", name)
	}

	t.status.ActiveProfile = ""
	t.status.ActiveProfileIndex = -1
	t.appendHistory(event)
	t.emit()
}

func (t *statusTracker) appendHistory(event Event) {
	if t.historyLimit < 0 {
		return
	}

	t.status.History = append(t.status.History, event)
	if len(t.status.History) > t.historyLimit {
		t.status.History = t.status.History[len(t.status.History)-t.historyLimit:]
	}
}

func (t *statusTracker) emit() {
	if t.notify == nil {
		return
	}

	t.notify(t.snapshot())
}

func (t *statusTracker) snapshot() Status {
	status := t.status
	status.Profiles = make([]ProfileStatus, 0, len(t.byName))
	for _, profile := range t.byName {
		status.Profiles = append(status.Profiles, *profile)
	}
	status.History = append([]Event(nil), t.status.History...)

	return status
}

func waitRetryDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("retry delay canceled: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
