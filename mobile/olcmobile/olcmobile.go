// Package olcmobile exposes the minimal gomobile API used by the iOS tunnel.
package olcmobile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/app/session"
	configpkg "github.com/openlibrecommunity/olcrtc/internal/config"
)

var (
	mu                  sync.Mutex
	cancel              context.CancelFunc
	ready               chan struct{}
	done                chan struct{}
	errRun              error
	activeRunID         uint64
	sessionRunWithReady = session.RunWithReady //nolint:gochecknoglobals // tests replace long-running runner
	runSessionWithReady = sessionRunWithReady
)

var (
	errNotRunning         = errors.New("olcRTC is not running")
	errStoppedBeforeReady = errors.New("olcRTC stopped before becoming ready")
	errStartTimedOut      = errors.New("olcRTC start timed out")
)

// StartCnc starts the CNC tunnel from a YAML config and blocks until Stop or error.
func StartCnc(configYAML, dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	cfgPath := filepath.Join(dataDir, "cnc.yaml")
	if err := os.WriteFile(cfgPath, []byte(configYAML), 0o600); err != nil {
		return err
	}

	session.RegisterDefaults()
	f, err := configpkg.Load(cfgPath)
	if err != nil {
		return err
	}
	scfg := configpkg.Apply(session.Config{}, f)
	if scfg, err = session.ApplyAuthDefaults(scfg); err != nil {
		return err
	}
	scfg = session.ApplyTransportDefaults(scfg)
	scfg = session.ApplyLivenessDefaults(scfg)
	if err := session.Validate(scfg); err != nil {
		return err
	}

	ctx, c := context.WithCancel(context.Background())
	localReady := make(chan struct{})
	localDone := make(chan struct{})
	mu.Lock()
	if cancel != nil {
		cancel()
	}
	activeRunID++
	runID := activeRunID
	cancel = c
	ready = localReady
	done = localDone
	errRun = nil
	mu.Unlock()

	var readyOnce sync.Once
	err = runSessionWithReady(ctx, scfg, func() {
		readyOnce.Do(func() {
			close(localReady)
		})
	})

	mu.Lock()
	if activeRunID == runID {
		cancel = nil
		errRun = err
	}
	mu.Unlock()
	close(localDone)
	return err
}

// WaitReady blocks until StartCnc has established the client session and SOCKS
// listener, the StartCnc goroutine exits, or the timeout elapses.
func WaitReady(timeoutMillis int) error {
	if timeoutMillis <= 0 {
		return waitReadySnapshot()
	}

	timer := time.NewTimer(time.Duration(timeoutMillis) * time.Millisecond)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		err, pending := waitReadySnapshotPending()
		if !pending {
			return err
		}

		select {
		case <-timer.C:
			return errStartTimedOut
		case <-ticker.C:
		}
	}
}

func waitReadySnapshot() error {
	err, _ := waitReadySnapshotPending()
	return err
}

func waitReadySnapshotPending() (error, bool) {
	mu.Lock()
	r := ready
	d := done
	runErr := errRun
	running := cancel != nil
	mu.Unlock()

	if r == nil {
		if runErr != nil {
			return runErr, false
		}
		return errNotRunning, true
	}

	select {
	case <-r:
		return nil, false
	default:
	}

	if !running {
		if runErr != nil {
			return runErr, false
		}
		return errStoppedBeforeReady, false
	}

	select {
	case <-r:
		return nil, false
	case <-d:
		mu.Lock()
		runErr = errRun
		mu.Unlock()
		if runErr != nil {
			return runErr, false
		}
		return errStoppedBeforeReady, false
	default:
		return nil, true
	}
}

// Stop cancels the active tunnel.
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if cancel != nil {
		cancel()
		cancel = nil
	}
}
