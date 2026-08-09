package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/openlibrecommunity/olcrtc/internal/app/session"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/supervisor"
)

var errBoom = errors.New("boom")

const (
	testAuthWBStream = "wbstream"
	testDNSServer    = "8.8.8.8:53"
)

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "olcrtc.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return path
}

func TestRunWithArgsRequiresConfig(t *testing.T) {
	session.RegisterDefaults()
	if err := runWithArgs(nil); !errors.Is(err, ErrConfigPathRequired) {
		t.Fatalf("runWithArgs(nil) = %v, want %v", err, ErrConfigPathRequired)
	}
	if err := runWithArgs([]string{"-h"}); !errors.Is(err, ErrConfigPathRequired) {
		t.Fatalf("runWithArgs(-h) = %v, want %v", err, ErrConfigPathRequired)
	}
	if err := runWithArgs([]string{"a.yaml", "b.yaml"}); !errors.Is(err, ErrConfigPathRequired) {
		t.Fatalf("runWithArgs(two args) = %v, want %v", err, ErrConfigPathRequired)
	}
}

func TestRunGenModeValidationErrors(t *testing.T) {
	session.RegisterDefaults()

	if err := runWithConfig(loadedConfig{scfg: session.Config{Mode: modeGen}}); err == nil {
		t.Fatal("runWithConfig(gen, no carrier) error = nil")
	}

	cfg := loadedConfig{scfg: session.Config{
		Mode: modeGen, Auth: testAuthWBStream, DNSServer: testDNSServer,
	}}
	if err := runWithConfig(cfg); err == nil {
		t.Fatal("runWithConfig(gen, amount=0) error = nil")
	}
}

func TestRunGenModeCallsGen(t *testing.T) {
	session.RegisterDefaults()

	var collected []string
	oldRunGen := runGen
	t.Cleanup(func() { runGen = oldRunGen })
	runGen = func(scfg session.Config) error {
		if scfg.Auth != testAuthWBStream || scfg.DNSServer != testDNSServer || scfg.Amount != 3 {
			t.Fatalf("runGen scfg = %+v", scfg)
		}
		collected = append(collected, "ok")
		return nil
	}

	cfg := loadedConfig{scfg: session.Config{
		Mode: modeGen, Auth: testAuthWBStream, DNSServer: testDNSServer, Amount: 3,
	}}
	if err := runWithConfig(cfg); err != nil {
		t.Fatalf("runWithConfig(gen) error = %v", err)
	}
	if len(collected) != 1 {
		t.Fatalf("runGen called %d times, want 1", len(collected))
	}
}

func TestRunWithConfigValidationAndDataDirErrors(t *testing.T) {
	session.RegisterDefaults()
	scfg := session.Config{
		Mode:      "srv",
		Transport: "datachannel",
		Auth:      "jitsi",
		RoomID:    "https://meet.systemli.org/test",
		KeyHex:    "key",
		DNSServer: "8.8.8.8:53",
	}
	if err := runWithConfig(loadedConfig{scfg: scfg}); !errors.Is(err, ErrDataDirRequired) {
		t.Fatalf("runWithConfig(no data dir) = %v, want %v", err, ErrDataDirRequired)
	}

	scfg.Mode = ""
	if err := runWithConfig(loadedConfig{scfg: scfg}); err == nil {
		t.Fatal("runWithConfig(invalid config) error = nil")
	}
}

func TestRunWithArgsSuccessfulSessionReturn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "names"), []byte("A\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(names) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "surnames"), []byte("B\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(surnames) error = %v", err)
	}

	oldRunSession := runSession
	t.Cleanup(func() {
		runSession = oldRunSession
	})
	called := false
	runSession = func(ctx context.Context, cfg session.Config) error {
		called = true
		if cfg.Mode != "srv" || cfg.Auth != "jitsi" {
			t.Fatalf("session config = %+v", cfg)
		}
		select {
		case <-ctx.Done():
			t.Fatal("context canceled before session returned")
		default:
		}
		return nil
	}

	yamlPath := writeYAML(t, `
mode: srv
link: direct
auth:
  provider: jitsi
room:
  id: https://meet.systemli.org/test
crypto:
  key: key
net:
  transport: datachannel
  dns: 8.8.8.8:53
data: `+dir+`
`)

	if err := runWithArgs([]string{yamlPath}); err != nil {
		t.Fatalf("runWithArgs() error = %v", err)
	}
	if !called {
		t.Fatal("runWithArgs() did not call session runner")
	}
}

func TestRunWithArgsAppliesTransportDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "names"), []byte("A\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(names) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "surnames"), []byte("B\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(surnames) error = %v", err)
	}

	oldRunSession := runSession
	t.Cleanup(func() { runSession = oldRunSession })
	runSession = func(_ context.Context, cfg session.Config) error {
		if cfg.VP8.FPS != 30 || cfg.VP8.BatchSize != 64 {
			t.Errorf("VP8 defaults = fps %d batch %d, want 30/64", cfg.VP8.FPS, cfg.VP8.BatchSize)
		}
		return nil
	}

	yamlPath := writeYAML(t, `
mode: srv
link: direct
auth:
  provider: wbstream
room:
  id: room
crypto:
  key: key
net:
  transport: vp8channel
  dns: 8.8.8.8:53
data: `+dir+`
`)

	if err := runWithArgs([]string{yamlPath}); err != nil {
		t.Fatalf("runWithArgs() error = %v", err)
	}
}

func TestRunWithArgsFailoverProfiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "names"), []byte("A\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(names) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "surnames"), []byte("B\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(surnames) error = %v", err)
	}

	oldRunSession := runSession
	t.Cleanup(func() { runSession = oldRunSession })
	var seen []string
	runSession = func(_ context.Context, cfg session.Config) error {
		seen = append(seen, cfg.Auth+"/"+cfg.Transport)
		if cfg.Auth == "wbstream" && (cfg.VP8.FPS != 30 || cfg.VP8.BatchSize != 64) {
			t.Errorf("VP8 defaults = fps %d batch %d, want 30/64", cfg.VP8.FPS, cfg.VP8.BatchSize)
		}
		return errBoom
	}

	yamlPath := writeYAML(t, `
mode: srv
link: direct
crypto:
  key: key
net:
  dns: 8.8.8.8:53
profiles:
  - name: wb-primary
    auth:
      provider: wbstream
    room:
      id: room
    net:
      transport: vp8channel
  - name: jitsi-backup
    auth:
      provider: jitsi
    room:
      id: https://meet.example/room
    net:
      transport: datachannel
failover:
  retry_delay: -1ns
  max_cycles: 1
data: `+dir+`
`)

	err := runWithArgs([]string{yamlPath})
	if !errors.Is(err, supervisor.ErrMaxCyclesExceeded) {
		t.Fatalf("runWithArgs() error = %v, want %v", err, supervisor.ErrMaxCyclesExceeded)
	}
	want := []string{"wbstream/vp8channel", "jitsi/datachannel"}
	if !equalStrings(seen, want) {
		t.Fatalf("seen profiles = %v, want %v", seen, want)
	}
}

func TestRunWithConfigRejectsProfilesInGenMode(t *testing.T) {
	cfg := loadedConfig{
		scfg:     session.Config{Mode: modeGen},
		profiles: []supervisor.Profile{{Name: "one"}},
	}
	if err := runWithConfig(cfg); !errors.Is(err, ErrProfilesUnsupportedForGen) {
		t.Fatalf("runWithConfig() error = %v, want %v", err, ErrProfilesUnsupportedForGen)
	}
}

func TestConfigureLogging(t *testing.T) {
	t.Setenv("PION_LOG_DISABLE", "")
	logger.SetVerbose(false)
	configureLogging(true)
	if !logger.IsVerbose() {
		t.Fatal("configureLogging(true) did not enable verbose logging")
	}
	if got := os.Getenv("PION_LOG_DISABLE"); got != "turnc" {
		t.Fatalf("configureLogging(true) PION_LOG_DISABLE = %q, want turnc", got)
	}

	logger.SetVerbose(false)
	configureLogging(false)
	if logger.IsVerbose() {
		t.Fatal("configureLogging(false) enabled verbose logging")
	}
	if got := os.Getenv("PION_LOG_DISABLE"); got != "all" {
		t.Fatalf("configureLogging(false) PION_LOG_DISABLE = %q, want all", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestResolveDataDir(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "data")
	got, err := resolveDataDir(abs)
	if err != nil {
		t.Fatalf("resolveDataDir(abs) error = %v", err)
	}
	if got != abs {
		t.Fatalf("resolveDataDir(abs) = %q, want %q", got, abs)
	}

	got, err = resolveDataDir("data")
	if err != nil {
		t.Fatalf("resolveDataDir(rel) error = %v", err)
	}
	if filepath.Base(got) != "data" || !filepath.IsAbs(got) {
		t.Fatalf("resolveDataDir(rel) = %q, want absolute path ending in data", got)
	}
}

func TestLoadNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "names"), []byte("A\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(names) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "surnames"), []byte("B\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(surnames) error = %v", err)
	}
	if err := loadNames(dir); err != nil {
		t.Fatalf("loadNames() error = %v", err)
	}
}

func TestWaitForShutdown(t *testing.T) {
	errCh := make(chan error, 1)
	errCh <- nil
	if err := waitForShutdown(errCh); err != nil {
		t.Fatalf("waitForShutdown(nil) error = %v", err)
	}

	want := errBoom
	errCh = make(chan error, 1)
	errCh <- want
	if err := waitForShutdown(errCh); !errors.Is(err, want) {
		t.Fatalf("waitForShutdown(error) = %v, want %v", err, want)
	}
}
