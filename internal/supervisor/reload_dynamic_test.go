package supervisor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/app/session"
)

// TestRunDynamicReloadFollowsRollingWindow proves that when the profile list is
// reloaded on every advance, the supervisor follows a rolling window: after the
// room it just left drops out of the list, it lands on the new head, so the
// sequence of rooms it runs is R1,R2,R3,... even though each was only added to
// the list after the previous one had already started.
func TestRunDynamicReloadFollowsRollingWindow(t *testing.T) {
	prof := func(name string) Profile {
		return Profile{Name: name, Config: session.Config{RoomID: name}}
	}

	windows := [][]Profile{
		{prof("R1")},
		{prof("R1"), prof("R2")},
		{prof("R2"), prof("R3")},
		{prof("R3"), prof("R4")},
		{prof("R4"), prof("R5")},
	}

	var mu sync.Mutex
	step := 0
	var got []string

	reload := func() ([]Profile, error) {
		mu.Lock()
		defer mu.Unlock()
		if step >= len(windows) {
			return windows[len(windows)-1], nil
		}
		return windows[step], nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	run := func(_ context.Context, cfg session.Config) error {
		mu.Lock()
		got = append(got, cfg.RoomID)
		if step < len(windows) {
			step++
		}
		done := len(got) >= 5
		mu.Unlock()
		if done {
			cancel()
		}
		return nil // clean end -> advance to the next room in the (reloaded) list
	}

	if err := Run(ctx, Config{
		Profiles:   windows[0],
		Reload:     reload,
		RetryDelay: time.Millisecond,
	}, run); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	want := []string{"R1", "R2", "R3", "R4", "R5"}
	mu.Lock()
	defer mu.Unlock()
	if len(got) < len(want) {
		t.Fatalf("got %v, want at least %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d: got %q want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}
