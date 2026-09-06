package mobile

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/pkg/olcrtc/client"
)

const (
	testRoom = "https://meet.example.org/room"
	testKey  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

//nolint:gochecknoglobals,nolintlint // shared immutable sentinel keeps errors.Is assertions precise
var errTestRun = errors.New("test runner failed")

func configuredRuntime(t *testing.T, runner clientRunner) *Runtime {
	t.Helper()
	runtime := newRuntime(runner)
	if err := runtime.SetProvider("jitsi"); err != nil {
		t.Fatalf("SetProvider() error = %v", err)
	}
	if err := runtime.SetRoom(testRoom); err != nil {
		t.Fatalf("SetRoom() error = %v", err)
	}
	if err := runtime.SetKey(testKey); err != nil {
		t.Fatalf("SetKey() error = %v", err)
	}
	return runtime
}

func blockingReadyRunner(ctx context.Context, _ client.Config, onReady func(string)) error {
	onReady("127.0.0.1:1080")
	<-ctx.Done()
	return ctx.Err()
}

func TestRuntimeLifecycle(t *testing.T) {
	runtime := configuredRuntime(t, blockingReadyRunner)
	if runtime.State() != "idle" {
		t.Fatalf("State() = %q, want idle", runtime.State())
	}
	if err := runtime.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.Start(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Start() error = %v, want %v", err, ErrAlreadyRunning)
	}
	if err := runtime.WaitReady(100); err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	if runtime.State() != "running" || !runtime.IsRunning() {
		t.Fatalf("state after ready = %q, running = %v", runtime.State(), runtime.IsRunning())
	}
	if err := runtime.Stop(100); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if runtime.State() != "stopped" || runtime.IsRunning() {
		t.Fatalf("state after stop = %q, running = %v", runtime.State(), runtime.IsRunning())
	}
	if err := runtime.Stop(1); err != nil {
		t.Fatalf("idempotent Stop() error = %v", err)
	}
}

func TestWaitReadyReturnsStartupError(t *testing.T) {
	runtime := configuredRuntime(t, func(context.Context, client.Config, func(string)) error {
		return errTestRun
	})
	if err := runtime.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.WaitReady(100); !errors.Is(err, errTestRun) {
		t.Fatalf("WaitReady() error = %v, want %v", err, errTestRun)
	}
}

func TestWaitReadyTimeout(t *testing.T) {
	runtime := configuredRuntime(t, func(ctx context.Context, _ client.Config, _ func(string)) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if err := runtime.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.WaitReady(1); !errors.Is(err, ErrReadyTimeout) {
		t.Fatalf("WaitReady() error = %v, want %v", err, ErrReadyTimeout)
	}
	if err := runtime.Stop(100); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestTimeoutConversionDoesNotOverflow(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	if got := timeoutFromMillis(maxInt, time.Second); got <= 0 {
		t.Fatalf("timeoutFromMillis(maxInt) = %v", got)
	}
}

func TestStopTimeoutKeepsStoppingState(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	runtime := configuredRuntime(t, func(context.Context, client.Config, func(string)) error {
		close(started)
		<-release
		return nil
	})
	if err := runtime.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	// Stop has to find the runner ALIVE for this test to mean anything. A stop
	// that lands before the session starts is honoured without running it at
	// all - and then there is nothing to time out on.
	<-started
	if err := runtime.Stop(1); !errors.Is(err, ErrStopTimeout) {
		t.Fatalf("Stop() error = %v, want %v", err, ErrStopTimeout)
	}
	if runtime.State() != "stopping" {
		t.Fatalf("State() = %q, want stopping", runtime.State())
	}
	close(release)
	waitForState(t, runtime, "stopped")
}

func TestRapidRestartUsesNewGenerations(t *testing.T) {
	runtime := configuredRuntime(t, blockingReadyRunner)
	for generation := range 25 {
		if err := runtime.Start(); err != nil {
			t.Fatalf("Start() generation %d error = %v", generation, err)
		}
		if err := runtime.WaitReady(100); err != nil {
			t.Fatalf("WaitReady() generation %d error = %v", generation, err)
		}
		if err := runtime.Stop(100); err != nil {
			t.Fatalf("Stop() generation %d error = %v", generation, err)
		}
	}
	runtime.mu.Lock()
	got := runtime.nextGeneration
	runtime.mu.Unlock()
	if got != 25 {
		t.Fatalf("generation ID = %d, want 25", got)
	}
}

func TestStaleWaiterCannotObserveRestart(t *testing.T) {
	var calls int
	var callsMu sync.Mutex
	firstStarted := make(chan struct{})
	runtime := configuredRuntime(t, func(ctx context.Context, _ client.Config, onReady func(string)) error {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		if call == 1 {
			close(firstStarted)
		}
		if call > 1 {
			onReady("127.0.0.1:1080")
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if err := runtime.Start(); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	// The first session must actually be running before it is stopped. A stop
	// that lands first is honoured without a run, and the call count the runner
	// keys its behaviour on would then be off by one.
	<-firstStarted
	runtime.mu.Lock()
	first := runtime.current
	runtime.mu.Unlock()
	staleResult := make(chan error, 1)
	go func() { staleResult <- runtime.waitGenerationReady(first, time.Second) }()
	if err := runtime.Stop(100); err != nil {
		t.Fatalf("first Stop() error = %v", err)
	}
	if err := runtime.Start(); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	if err := runtime.WaitReady(100); err != nil {
		t.Fatalf("second WaitReady() error = %v", err)
	}
	if err := <-staleResult; !errors.Is(err, ErrStoppedBeforeReady) {
		t.Fatalf("stale waiter error = %v, want %v", err, ErrStoppedBeforeReady)
	}
	if err := runtime.Stop(100); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
}

func TestConcurrentStartStopWaitReady(t *testing.T) {
	runtime := configuredRuntime(t, blockingReadyRunner)
	if err := runtime.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			err := runtime.Start()
			if err != nil && !errors.Is(err, ErrAlreadyRunning) {
				t.Errorf("concurrent Start() error = %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			err := runtime.WaitReady(100)
			// A generation that stopped is reported as not running when it
			// had become ready, and as stopped-before-ready otherwise.
			if err != nil && !errors.Is(err, ErrStoppedBeforeReady) && !errors.Is(err, ErrNotRunning) {
				t.Errorf("concurrent WaitReady() error = %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := runtime.Stop(100); err != nil {
				t.Errorf("concurrent Stop() error = %v", err)
			}
		}()
	}
	wg.Wait()
	if err := runtime.Stop(100); err != nil {
		t.Fatalf("final Stop() error = %v", err)
	}
	waitForState(t, runtime, "stopped")
}

func waitForState(t *testing.T, runtime *Runtime, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if runtime.State() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("State() = %q, want %q", runtime.State(), want)
}

// TestWaitReadyAfterStop locks in that readiness is not reported for a
// runtime that has stopped. The ready channel of the finished generation
// stays closed and the runtime keeps that generation, so checking the latch
// alone told a caller the tunnel was up while State() said stopped.
func TestWaitReadyAfterStop(t *testing.T) {
	runtime := configuredRuntime(t, blockingReadyRunner)
	if err := runtime.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.WaitReady(1000); err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	if err := runtime.Stop(1000); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := runtime.WaitReady(100); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("WaitReady() after Stop = %v, want %v", err, ErrNotRunning)
	}
	if state := runtime.State(); state != "stopped" {
		t.Fatalf("State() = %q, want stopped", state)
	}
}

// A retired primary must not strand the client: the standby delivered
// alongside it is tried next. This is the whole point of the failover list.
func TestFailoverAdvancesToNextRoomWhenTheFirstEnds(t *testing.T) {
	prev := failoverRetryDelay
	failoverRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { failoverRetryDelay = prev })

	const standby = "https://meet.example.org/standby"
	var seenMu sync.Mutex
	var seen []string
	runtime := configuredRuntime(t, func(ctx context.Context, cfg client.Config, onReady func(string)) error {
		seenMu.Lock()
		seen = append(seen, cfg.RoomURL)
		seenMu.Unlock()
		if cfg.RoomURL == testRoom {
			return errTestRun // the primary is gone - what a retired room looks like
		}
		onReady("127.0.0.1:1080")
		<-ctx.Done()
		return ctx.Err()
	})
	if err := runtime.AddFailoverRoom(standby); err != nil {
		t.Fatalf("AddFailoverRoom() error = %v", err)
	}
	if err := runtime.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.WaitReady(2000); err != nil {
		t.Fatalf("WaitReady() error = %v, want the standby to come up", err)
	}
	seenMu.Lock()
	got := append([]string(nil), seen...)
	seenMu.Unlock()
	if len(got) != 2 || got[0] != testRoom || got[1] != standby {
		t.Fatalf("rooms tried = %v, want [%s %s]", got, testRoom, standby)
	}
	if err := runtime.Stop(100); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

// A room delivered while a session is live - as a subscription refresh does -
// is used at the next hop, without a restart. Without this the list a client
// starts with is the only list it ever has.
func TestRoomsAddedDuringASessionAreUsedAtTheNextHop(t *testing.T) {
	prev := failoverRetryDelay
	failoverRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { failoverRetryDelay = prev })

	const standby = "https://meet.example.org/standby"
	retirePrimary := make(chan struct{})
	standbyRunning := make(chan struct{})
	runtime := configuredRuntime(t, func(ctx context.Context, cfg client.Config, onReady func(string)) error {
		onReady("127.0.0.1:1080")
		if cfg.RoomURL == testRoom {
			<-retirePrimary // the server retires it while we sit in it
			return nil
		}
		close(standbyRunning)
		<-ctx.Done()
		return ctx.Err()
	})
	if err := runtime.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.WaitReady(100); err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	if err := runtime.AddFailoverRoom(standby); err != nil {
		t.Fatalf("AddFailoverRoom() error = %v", err)
	}
	close(retirePrimary)
	select {
	case <-standbyRunning:
	case <-time.After(2 * time.Second):
		t.Fatal("the room added mid-session was never tried")
	}
	if runtime.State() != "running" {
		t.Fatalf("State() = %q after the hop, want running", runtime.State())
	}
	if err := runtime.Stop(100); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestFailoverRoomListIsOrderedAndDeduplicated(t *testing.T) {
	runtime := configuredRuntime(t, blockingReadyRunner)
	if err := runtime.AddFailoverRoom("  "); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("blank AddFailoverRoom() error = %v, want %v", err, ErrInvalidConfig)
	}
	for _, room := range []string{"b", testRoom, "a", "b"} {
		if err := runtime.AddFailoverRoom(room); err != nil {
			t.Fatalf("AddFailoverRoom(%q) error = %v", room, err)
		}
	}
	want := []string{testRoom, "b", "a"}
	got := runtime.defaults.rooms()
	if len(got) != len(want) {
		t.Fatalf("rooms() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rooms() = %v, want %v", got, want)
		}
	}
	runtime.ClearFailoverRooms()
	if got := runtime.defaults.rooms(); len(got) != 1 || got[0] != testRoom {
		t.Fatalf("rooms() after clear = %v, want [%s]", got, testRoom)
	}
}

// The host app cannot read the runtime's logs, so the moment a session is
// established - the desktop client's "session opened" line - reaches it
// through a listener instead, naming the room. It fires for every session, so
// a failover to another room is visible as exactly that.
func TestSessionListenerNamesTheRoomOfEachSession(t *testing.T) {
	prev := failoverRetryDelay
	failoverRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { failoverRetryDelay = prev })

	const standby = "https://meet.example.org/standby"
	runtime := configuredRuntime(t, func(ctx context.Context, cfg client.Config, onReady func(string)) error {
		cfg.OnSessionOpen("session-in-" + cfg.RoomURL)
		if cfg.RoomURL == testRoom {
			return errTestRun
		}
		onReady("127.0.0.1:1080")
		<-ctx.Done()
		return ctx.Err()
	})
	listener := &recordingListener{}
	runtime.SetSessionListener(listener)
	if err := runtime.AddFailoverRoom(standby); err != nil {
		t.Fatalf("AddFailoverRoom() error = %v", err)
	}
	if err := runtime.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.WaitReady(2000); err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	want := []string{
		testRoom + " session-in-" + testRoom,
		standby + " session-in-" + standby,
	}
	if got := listener.events(); !reflect.DeepEqual(got, want) {
		t.Fatalf("session events = %v, want %v", got, want)
	}
	if err := runtime.Stop(100); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

// A runtime with no listener installed still lets the client report sessions;
// the events simply go nowhere. A listener installed later hears the next one,
// without a restart.
func TestSessionListenerIsOptionalAndReplaceable(t *testing.T) {
	sessions := make(chan struct{}, 1)
	runtime := configuredRuntime(t, func(ctx context.Context, cfg client.Config, onReady func(string)) error {
		cfg.OnSessionOpen("first")
		onReady("127.0.0.1:1080")
		<-sessions
		cfg.OnSessionOpen("second")
		<-ctx.Done()
		return ctx.Err()
	})
	if err := runtime.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.WaitReady(2000); err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	listener := &recordingListener{}
	runtime.SetSessionListener(listener)
	sessions <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for len(listener.events()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got, want := listener.events(), []string{testRoom + " second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("session events = %v, want %v", got, want)
	}
	if err := runtime.Stop(100); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

type recordingListener struct {
	mu     sync.Mutex
	opened []string
}

func (l *recordingListener) OnSessionOpened(room, sessionID string) {
	l.mu.Lock()
	l.opened = append(l.opened, room+" "+sessionID)
	l.mu.Unlock()
}

func (l *recordingListener) events() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.opened...)
}
