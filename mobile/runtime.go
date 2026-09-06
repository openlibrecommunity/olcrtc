// Package mobile provides a gomobile-compatible olcRTC client API.
package mobile

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/app/session"
	"github.com/openlibrecommunity/olcrtc/internal/supervisor"
	"github.com/openlibrecommunity/olcrtc/pkg/olcrtc/client"

	_ "golang.org/x/mobile/bind"                       // keep gomobile binding dependencies reachable
	_ "google.golang.org/genproto/protobuf/field_mask" // keep gomobile on post-split genproto modules
)

const (
	stateIdle     runtimeState = "idle"
	stateStarting runtimeState = "starting"
	stateRunning  runtimeState = "running"
	stateStopping runtimeState = "stopping"
	stateStopped  runtimeState = "stopped"
)

const (
	defaultReadyTimeout = 8 * time.Second
	defaultStopTimeout  = 5 * time.Second
	maxTimeoutMillis    = int64(9223372036854)
)

//nolint:gochecknoglobals,nolintlint // sentinel identities are immutable public API values
var (
	ErrAlreadyRunning       = errors.New("olcRTC runtime is already active")
	ErrNotRunning           = errors.New("olcRTC runtime has not been started")
	ErrStoppedBeforeReady   = errors.New("olcRTC runtime stopped before becoming ready")
	ErrReadyTimeout         = errors.New("olcRTC runtime readiness timed out")
	ErrStopTimeout          = errors.New("olcRTC runtime stop timed out")
	ErrInvalidConfig        = errors.New("invalid mobile runtime configuration")
	ErrUnsupportedProvider  = errors.New("unsupported provider")
	ErrUnsupportedTransport = errors.New("unsupported transport")
)

type runtimeState string

type clientRunner func(context.Context, client.Config, func(string)) error

type runGeneration struct {
	id            uint64
	cfg           client.Config
	cancel        context.CancelFunc
	ready         chan struct{}
	done          chan struct{}
	readyOnce     sync.Once
	doneOnce      sync.Once
	stopRequested bool
	err           error
}

// Runtime owns one independently configured mobile client lifecycle.
type Runtime struct {
	mu             sync.Mutex
	defaults       runtimeConfig
	state          runtimeState
	nextGeneration uint64
	current        *runGeneration
	runner         clientRunner
	listener       SessionListener
}

// New returns an idle Runtime with documented mobile defaults.
func New() *Runtime {
	return newRuntime(runPublicClient)
}

func newRuntime(runner clientRunner) *Runtime {
	client.RegisterDefaults()
	return &Runtime{
		defaults: defaultRuntimeConfig(),
		state:    stateIdle,
		runner:   runner,
	}
}

func runPublicClient(ctx context.Context, cfg client.Config, onReady func(string)) error {
	if err := client.New(cfg).RunWithAddress(ctx, onReady); err != nil {
		return fmt.Errorf("run public client: %w", err)
	}
	return nil
}

// Start launches a new generation from the current configuration snapshot.
func (r *Runtime) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.isActiveLocked() {
		return ErrAlreadyRunning
	}
	if err := validateRuntimeConfig(r.defaults); err != nil {
		return err
	}
	cfg := r.defaults.clientConfig()

	ctx, cancel := context.WithCancel(context.Background())
	r.nextGeneration++
	gen := &runGeneration{
		id:     r.nextGeneration,
		cfg:    cfg,
		cancel: cancel,
		ready:  make(chan struct{}),
		done:   make(chan struct{}),
	}
	r.current = gen
	r.state = stateStarting
	go r.run(ctx, gen)
	return nil
}

// Failover across rooms happens inside one generation, the way the desktop
// client does it under its supervisor: when the room a session is in ends -
// the server retired it, or it died - the next room in the list is tried, and
// rooms added while a session was live are seen at that moment.
//
// What the mobile runtime deliberately does NOT do is retry forever on its own.
// It makes one forward pass over the list, extended by whatever the host app
// appends meanwhile, and then the generation ends with an error. The host app
// already owns a retry loop, and it re-evaluates the upstream network between
// attempts - on a phone that is the part that matters. A second, blind loop
// underneath it would only hide failures from the one that can act on them.
const failoverMaxCycles = 1

// A variable rather than a constant only so tests can shorten the wait between
// rooms; nothing in production changes it.
//
//nolint:gochecknoglobals // test-adjustable tunable
var failoverRetryDelay = 2 * time.Second

func (r *Runtime) run(ctx context.Context, gen *runGeneration) {
	onReady := func(string) { r.markReady(gen) }
	err := supervisor.Run(ctx, supervisor.Config{
		Profiles:   r.profilesSnapshot(),
		Reload:     func() ([]supervisor.Profile, error) { return r.profilesSnapshot(), nil },
		RetryDelay: failoverRetryDelay,
		MaxCycles:  failoverMaxCycles,
	}, func(ctx context.Context, profile session.Config) error {
		// Only the room varies between profiles. Everything else is the
		// generation's configuration snapshot, exactly as before failover.
		cfg := gen.cfg
		cfg.RoomURL = profile.RoomID
		cfg.OnSessionOpen = func(sessionID string) { r.notifySessionOpened(gen, profile.RoomID, sessionID) }
		return r.runner(ctx, cfg, onReady)
	})
	gen.cancel()
	r.finish(gen, err)
}

// profilesSnapshot is the supervisor's view of the room list: the primary
// first, then the failover extras, each a profile carrying only its room. Read
// under the lock, so a host app extending the list during a live session is
// seen at the next hop rather than the next Start.
func (r *Runtime) profilesSnapshot() []supervisor.Profile {
	r.mu.Lock()
	rooms := r.defaults.rooms()
	r.mu.Unlock()
	profiles := make([]supervisor.Profile, 0, len(rooms))
	for _, room := range rooms {
		profiles = append(profiles, supervisor.Profile{
			Name:   room,
			Config: session.Config{RoomID: room},
		})
	}
	return profiles
}

func (r *Runtime) markReady(gen *runGeneration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.isCurrentGenerationLocked(gen) || r.state != stateStarting {
		return
	}
	gen.readyOnce.Do(func() {
		r.state = stateRunning
		close(gen.ready)
	})
}

func (r *Runtime) finish(gen *runGeneration, runErr error) {
	r.mu.Lock()
	if gen.stopRequested && errors.Is(runErr, context.Canceled) {
		runErr = nil
	}
	gen.err = runErr
	if r.isCurrentGenerationLocked(gen) {
		r.state = stateStopped
	}
	r.mu.Unlock()
	gen.doneOnce.Do(func() { close(gen.done) })
}

// WaitReady waits for the generation current when the method is called.
func (r *Runtime) WaitReady(timeoutMillis int) error {
	r.mu.Lock()
	gen := r.current
	r.mu.Unlock()
	if gen == nil {
		return ErrNotRunning
	}
	return r.waitGenerationReady(gen, timeoutFromMillis(timeoutMillis, defaultReadyTimeout))
}

func (r *Runtime) waitGenerationReady(gen *runGeneration, timeout time.Duration) error {
	// A generation that has already exited is never ready, whatever its latch
	// says. The ready channel stays closed after Stop and the runtime keeps
	// the generation, so checking the latch alone reported a live tunnel for
	// a runtime whose State() already said stopped.
	if channelClosed(gen.done) {
		return r.generationError(gen)
	}
	if channelClosed(gen.ready) {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-gen.ready:
		return nil
	case <-gen.done:
		return r.generationError(gen)
	case <-timer.C:
		return ErrReadyTimeout
	}
}

// generationError describes a generation that has finished: its own failure
// when it had one, otherwise whether it ever reached readiness.
func (r *Runtime) generationError(gen *runGeneration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if gen.err != nil {
		return gen.err
	}
	if channelClosed(gen.ready) {
		return ErrNotRunning
	}
	return ErrStoppedBeforeReady
}

// Stop cancels the current generation and waits for its bounded shutdown.
func (r *Runtime) Stop(timeoutMillis int) error {
	r.mu.Lock()
	if r.state == stateIdle || r.state == stateStopped || r.current == nil {
		r.mu.Unlock()
		return nil
	}
	gen := r.current
	if r.state != stateStopping {
		r.state = stateStopping
		gen.stopRequested = true
		gen.cancel()
	}
	r.mu.Unlock()

	timer := time.NewTimer(timeoutFromMillis(timeoutMillis, defaultStopTimeout))
	defer timer.Stop()
	select {
	case <-gen.done:
		return nil
	case <-timer.C:
		return ErrStopTimeout
	}
}

// State returns idle, starting, running, stopping, or stopped.
func (r *Runtime) State() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.state)
}

// IsRunning reports whether a generation is starting, running, or stopping.
func (r *Runtime) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isActiveLocked()
}

func (r *Runtime) isActiveLocked() bool {
	return r.state == stateStarting || r.state == stateRunning || r.state == stateStopping
}

func (r *Runtime) isCurrentGenerationLocked(gen *runGeneration) bool {
	return r.current == gen && r.current.id == gen.id
}

func timeoutFromMillis(value int, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	if int64(value) > maxTimeoutMillis {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(value) * time.Millisecond
}

func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
