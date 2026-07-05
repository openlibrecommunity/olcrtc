package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/openlibrecommunity/olcrtc/internal/app/session"
)

const (
	testModeSrv      = "srv"
	testAuthProvider = "wbstream"
	testRoomID       = "r1"
	testCryptoKey    = "deadbeef"
	testDNSServer    = "8.8.8.8:53"
)

func TestLoadAndApply(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "olcrtc.yaml")
	body := `
mode: srv
link: direct
auth:
  provider: wbstream
room:
  id: r1
crypto:
  key: deadbeef
net:
  transport: datachannel
  dns: 8.8.8.8:53
socks:
  host: 127.0.0.1
  port: 1080
  user: u
  pass: p
vp8:
  fps: 25
  batch_size: 4
liveness:
  interval: 2s
  timeout: 500ms
  failures: 4
lifecycle:
  max_session_duration: 6h
traffic:
  max_payload_size: 4096
  min_delay: 5ms
  max_delay: 30ms
gen:
  amount: 3
debug: true
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	requireLoadedFile(t, f)

	got := Apply(session.Config{}, f)
	requireAppliedConfig(t, got)
}

func requireLoadedFile(t *testing.T, f File) {
	t.Helper()
	if f.Mode != testModeSrv {
		t.Fatalf("Mode = %q, want %q", f.Mode, testModeSrv)
	}
	if f.Auth.Provider != testAuthProvider {
		t.Fatalf("Auth.Provider = %q, want %q", f.Auth.Provider, testAuthProvider)
	}
	if f.Room.ID != testRoomID {
		t.Fatalf("Room.ID = %q, want %q", f.Room.ID, testRoomID)
	}
	if f.Crypto.Key != testCryptoKey {
		t.Fatalf("Crypto.Key = %q, want %q", f.Crypto.Key, testCryptoKey)
	}
}

func requireAppliedConfig(t *testing.T, got session.Config) {
	t.Helper()
	want := session.Config{
		Mode:                  testModeSrv,
		Auth:                  testAuthProvider,
		RoomID:                testRoomID,
		KeyHex:                testCryptoKey,
		Transport:             "datachannel",
		DNSServer:             testDNSServer,
		SOCKSHost:             "127.0.0.1",
		SOCKSPort:             1080,
		SOCKSUser:             "u",
		SOCKSPass:             "p",
		VP8:                   session.VP8Config{FPS: 25, BatchSize: 4},
		LivenessInterval:      "2s",
		LivenessTimeout:       "500ms",
		LivenessFailures:      4,
		MaxSessionDuration:    "6h",
		TrafficMaxPayloadSize: 4096,
		TrafficMinDelay:       "5ms",
		TrafficMaxDelay:       "30ms",
		Amount:                3,
	}
	if got != want {
		t.Fatalf("Apply produced wrong config: %+v, want %+v", got, want)
	}
}

func TestApplyCLIWins(t *testing.T) {
	cli := session.Config{
		Mode:      "cnc",
		KeyHex:    "from-cli",
		SOCKSPort: 9999,
	}
	f := File{
		Mode:   testModeSrv,
		Crypto: Crypto{Key: "from-yaml"},
		SOCKS:  SOCKS{Port: 1234, Host: "0.0.0.0"},
	}
	got := Apply(cli, f)
	if got.Mode != "cnc" {
		t.Errorf("Mode: got %q, want cnc (CLI wins)", got.Mode)
	}
	if got.KeyHex != "from-cli" {
		t.Errorf("KeyHex: got %q, want from-cli (CLI wins)", got.KeyHex)
	}
	if got.SOCKSPort != 9999 {
		t.Errorf("SOCKSPort: got %d, want 9999 (CLI wins)", got.SOCKSPort)
	}
	if got.SOCKSHost != "0.0.0.0" {
		t.Errorf("SOCKSHost: got %q, want 0.0.0.0 (YAML fills empty CLI)", got.SOCKSHost)
	}
}

//nolint:cyclop // profile merge fixture intentionally checks many mapped fields
func TestLoadAndApplyProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "olcrtc.yaml")
	body := `
mode: srv
link: direct
crypto:
  key: shared-key
net:
  dns: 8.8.8.8:53
liveness:
  interval: 5s
  timeout: 2s
  failures: 5
lifecycle:
  max_session_duration: 6h
traffic:
  max_payload_size: 8192
  min_delay: 10ms
  max_delay: 40ms
profiles:
  - name: wb-vp8
    auth:
      provider: wbstream
    room:
      id: wb-room
    net:
      transport: vp8channel
    vp8:
      fps: 30
    liveness:
      interval: 1s
    lifecycle:
      max_session_duration: 30m
    traffic:
      max_payload_size: 4096
      max_delay: 20ms
  - name: jitsi-dc
    auth:
      provider: jitsi
    room:
      id: https://meet.example/room
    net:
      transport: datachannel
      dns: 8.8.8.8:53
failover:
  retry_delay: 100ms
  max_cycles: 2
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Profiles) != 2 {
		t.Fatalf("profiles = %d, want 2", len(f.Profiles))
	}
	if f.Failover.RetryDelay != "100ms" || f.Failover.MaxCycles != 2 {
		t.Fatalf("Failover = %+v, want retry_delay 100ms max_cycles 2", f.Failover)
	}

	base := Apply(session.Config{}, f)
	first := ApplyProfile(base, f.Profiles[0])
	if first.Auth != "wbstream" || first.Transport != "vp8channel" || first.RoomID != "wb-room" {
		t.Fatalf("first profile = %+v", first)
	}
	if first.KeyHex != "shared-key" || first.DNSServer != testDNSServer || first.VP8.FPS != 30 ||
		first.LivenessInterval != "1s" || first.LivenessTimeout != "2s" || first.LivenessFailures != 5 ||
		first.MaxSessionDuration != "30m" || first.TrafficMaxPayloadSize != 4096 ||
		first.TrafficMinDelay != "10ms" || first.TrafficMaxDelay != "20ms" {
		t.Fatalf("first inherited/overlaid fields = %+v", first)
	}
	second := ApplyProfile(base, f.Profiles[1])
	if second.Auth != "jitsi" || second.Transport != "datachannel" ||
		second.RoomID != "https://meet.example/room" || second.DNSServer != testDNSServer {
		t.Fatalf("second profile = %+v", second)
	}
	if second.LivenessInterval != "5s" || second.LivenessTimeout != "2s" || second.LivenessFailures != 5 ||
		second.MaxSessionDuration != "6h" || second.TrafficMaxPayloadSize != 8192 ||
		second.TrafficMinDelay != "10ms" || second.TrafficMaxDelay != "40ms" {
		t.Fatalf("second lifecycle/liveness fields = %+v", second)
	}
}

func TestLoadProfileCryptoKeyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "profile.key"), []byte(testCryptoKey+"\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	path := filepath.Join(dir, "olcrtc.yaml")
	body := `
profiles:
  - name: file-key
    crypto:
      key_file: profile.key
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := f.Profiles[0].Crypto.Key; got != testCryptoKey {
		t.Fatalf("profile key = %q, want %q", got, testCryptoKey)
	}
}

func TestLoadCryptoKeyFileRelativeToConfig(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "secret.key")
	if err := os.WriteFile(keyPath, []byte(testCryptoKey+"\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	path := filepath.Join(dir, "olcrtc.yaml")
	body := `
mode: srv
crypto:
  key_file: secret.key
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.Crypto.Key != testCryptoKey {
		t.Fatalf("Crypto.Key = %q, want %q", f.Crypto.Key, testCryptoKey)
	}
}

func TestLoadCryptoKeyFileConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "olcrtc.yaml")
	body := `
crypto:
  key: deadbeef
  key_file: secret.key
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if !errors.Is(err, ErrCryptoKeyConflict) {
		t.Fatalf("Load() error = %v, want %v", err, ErrCryptoKeyConflict)
	}
}

func TestLoadCryptoKeyFileEmpty(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "secret.key")
	if err := os.WriteFile(keyPath, []byte("\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	path := filepath.Join(dir, "olcrtc.yaml")
	body := `
crypto:
  key_file: secret.key
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if !errors.Is(err, ErrCryptoKeyFileEmpty) {
		t.Fatalf("Load() error = %v, want %v", err, ErrCryptoKeyFileEmpty)
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadInvalidUTF8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "olcrtc.yaml")
	if err := os.WriteFile(path, []byte{'m', 'o', 'd', 'e', ':', ' ', 0xff}, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if !errors.Is(err, ErrConfigInvalidUTF8) {
		t.Fatalf("Load() error = %v, want invalid UTF-8 error", err)
	}
}
