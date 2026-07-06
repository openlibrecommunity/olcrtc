package olcmobile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/app/session"
)

var errOlcMobileRunProbe = errors.New("olcmobile run probe")

func resetGlobals(t *testing.T) {
	t.Helper()
	mu.Lock()
	if cancel != nil {
		cancel()
	}
	cancel = nil
	ready = nil
	done = nil
	errRun = nil
	activeRunID = 0
	runSessionWithReady = sessionRunWithReady
	mu.Unlock()
}

func TestStartCncWaitReadyUsesSessionReady(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })

	entered := make(chan struct{})
	release := make(chan struct{})
	runSessionWithReady = func(ctx context.Context, cfg session.Config, onReady func()) error {
		if cfg.Mode != "cnc" || cfg.SOCKSHost != "127.0.0.1" || cfg.SOCKSPort != 1080 {
			t.Fatalf("session config = %+v", cfg)
		}
		onReady()
		close(entered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return errOlcMobileRunProbe
		}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- StartCnc(testYAML(), t.TempDir())
	}()

	<-entered
	if err := WaitReady(1000); err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	close(release)
	if err := <-errCh; !errors.Is(err, errOlcMobileRunProbe) {
		t.Fatalf("StartCnc() error = %v, want %v", err, errOlcMobileRunProbe)
	}
}

func TestWaitReadyWaitsForStartCncToRegisterRun(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })

	entered := make(chan struct{})
	release := make(chan struct{})
	runSessionWithReady = func(ctx context.Context, _ session.Config, onReady func()) error {
		onReady()
		close(entered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return errOlcMobileRunProbe
		}
	}

	waitErrCh := make(chan error, 1)
	go func() {
		waitErrCh <- WaitReady(1000)
	}()
	time.Sleep(20 * time.Millisecond)

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- StartCnc(testYAML(), t.TempDir())
	}()

	<-entered
	if err := <-waitErrCh; err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	close(release)
	if err := <-runErrCh; !errors.Is(err, errOlcMobileRunProbe) {
		t.Fatalf("StartCnc() error = %v, want %v", err, errOlcMobileRunProbe)
	}
}

func TestWaitReadyReportsRunError(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })

	runSessionWithReady = func(context.Context, session.Config, func()) error {
		return errOlcMobileRunProbe
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- StartCnc(testYAML(), t.TempDir())
	}()
	if err := <-errCh; !errors.Is(err, errOlcMobileRunProbe) {
		t.Fatalf("StartCnc() error = %v, want %v", err, errOlcMobileRunProbe)
	}
	if err := WaitReady(1); !errors.Is(err, errOlcMobileRunProbe) {
		t.Fatalf("WaitReady() error = %v, want %v", err, errOlcMobileRunProbe)
	}
}

func TestStartCncWritesConfigBeforeRun(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })

	dataDir := t.TempDir()
	runSessionWithReady = func(context.Context, session.Config, func()) error {
		if _, err := os.Stat(filepath.Join(dataDir, "cnc.yaml")); err != nil {
			t.Fatalf("cnc.yaml was not written: %v", err)
		}
		return errOlcMobileRunProbe
	}
	if err := StartCnc(testYAML(), dataDir); !errors.Is(err, errOlcMobileRunProbe) {
		t.Fatalf("StartCnc() error = %v, want %v", err, errOlcMobileRunProbe)
	}
}

func TestOldStartCannotOverwriteCurrentRun(t *testing.T) {
	resetGlobals(t)
	t.Cleanup(func() { resetGlobals(t) })

	firstEntered := make(chan struct{})
	firstRelease := make(chan struct{})
	secondEntered := make(chan struct{})
	var calls atomic.Int32
	runSessionWithReady = func(ctx context.Context, _ session.Config, onReady func()) error {
		switch calls.Add(1) {
		case 1:
			close(firstEntered)
			<-firstRelease
			return errOlcMobileRunProbe
		case 2:
			onReady()
			close(secondEntered)
			<-ctx.Done()
			return ctx.Err()
		default:
			t.Fatal("unexpected extra StartCnc run")
			return nil
		}
	}

	firstErrCh := make(chan error, 1)
	go func() {
		firstErrCh <- StartCnc(testYAML(), t.TempDir())
	}()
	<-firstEntered

	secondErrCh := make(chan error, 1)
	go func() {
		secondErrCh <- StartCnc(testYAML(), t.TempDir())
	}()
	<-secondEntered

	close(firstRelease)
	if err := <-firstErrCh; !errors.Is(err, errOlcMobileRunProbe) {
		t.Fatalf("first StartCnc() error = %v, want %v", err, errOlcMobileRunProbe)
	}

	Stop()
	select {
	case err := <-secondErrCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("second StartCnc() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("second StartCnc() did not stop after Stop()")
	}
}

func testYAML() string {
	return `mode: cnc
auth:
  provider: none
room:
  id: room-1
crypto:
  key: "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 1080
`
}
