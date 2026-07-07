package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseArgsDefaultsToSafeWorkspacePaths(t *testing.T) {
	cfg, err := parseArgs([]string{"summarize", "--workspace", "/repo/whitelist-bypass", "--stamp", "run-1"})
	if err != nil {
		t.Fatalf("parseArgs() error = %v", err)
	}
	if cfg.Command != "summarize" {
		t.Fatalf("Command = %q, want summarize", cfg.Command)
	}
	if cfg.Workspace != "/repo/whitelist-bypass" {
		t.Fatalf("Workspace = %q", cfg.Workspace)
	}
	if cfg.Stamp != "run-1" {
		t.Fatalf("Stamp = %q", cfg.Stamp)
	}
	if cfg.Resolver != "8.8.8.8:53" {
		t.Fatalf("Resolver = %q", cfg.Resolver)
	}
	if cfg.Rounds != 3 {
		t.Fatalf("Rounds = %d", cfg.Rounds)
	}
	if cfg.DownloadBytes != 1048576 {
		t.Fatalf("DownloadBytes = %d", cfg.DownloadBytes)
	}
	if cfg.paths().SecretRunDir != "/repo/whitelist-bypass/.secrets/runtime/harness/run-1" {
		t.Fatalf("SecretRunDir = %q", cfg.paths().SecretRunDir)
	}
	if cfg.paths().ArtifactRunDir != "/repo/whitelist-bypass/artifacts/telemost-fix/harness/run-1" {
		t.Fatalf("ArtifactRunDir = %q", cfg.paths().ArtifactRunDir)
	}
}

func TestParseArgsRequiresDeviceForRunUnlessDryRun(t *testing.T) {
	if _, err := parseArgs([]string{"run", "--workspace", "/repo/whitelist-bypass", "--stamp", "run-1"}); err == nil {
		t.Fatal("parseArgs() error = nil, want missing device error")
	}
	if _, err := parseArgs([]string{"run", "--workspace", "/repo/whitelist-bypass", "--stamp", "run-1", "--dry-run"}); err != nil {
		t.Fatalf("parseArgs() dry-run error = %v", err)
	}
}

func TestRoomManagerEnvAddsCertifiBundleWhenUnset(t *testing.T) {
	env := roomManagerEnv([]string{"PATH=/bin"}, "/certifi/cacert.pem")
	want := "SSL_CERT_FILE=/certifi/cacert.pem"
	for _, got := range env {
		if got == want {
			return
		}
	}
	t.Fatalf("roomManagerEnv() = %v, missing %q", env, want)
}

func TestRoomManagerEnvDoesNotOverrideExistingSSLConfig(t *testing.T) {
	env := roomManagerEnv([]string{"PATH=/bin", "SSL_CERT_FILE=/custom.pem"}, "/certifi/cacert.pem")
	for _, got := range env {
		if got == "SSL_CERT_FILE=/certifi/cacert.pem" {
			t.Fatalf("roomManagerEnv() overrode SSL_CERT_FILE: %v", env)
		}
	}
}

func TestXcodeBuildArgsUseModernLinkerOverride(t *testing.T) {
	args := xcodeBuildArgs(paths{DerivedData: "/repo/artifacts/DerivedData"})
	for _, got := range args {
		if got == "OTHER_LDFLAGS=-lresolv" {
			return
		}
	}
	t.Fatalf("xcodeBuildArgs() = %v, missing modern linker override", args)
}

func TestStartServerReportsEarlyExit(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "server.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho failed to create transport >&2\nexit 7\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	logPath := filepath.Join(dir, "server.log")
	_, err := startServer(context.Background(), paths{
		ServerBinary: script,
		ServerConfig: filepath.Join(dir, "server.yaml"),
		ServerLog:    logPath,
	})
	if err == nil || !strings.Contains(err.Error(), "local server exited during startup") {
		t.Fatalf("startServer() error = %v, want startup exit error", err)
	}
	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read log: %v", readErr)
	}
	if !strings.Contains(string(data), "failed to create transport") {
		t.Fatalf("server log = %q, want early failure text", string(data))
	}
}

func TestStartServerWithRetryRestartsTransientAuthFailure(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "attempt")
	script := filepath.Join(dir, "server.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nn=0\nif [ -f "+state+" ]; then n=$(cat "+state+"); fi\nn=$((n+1))\necho \"$n\" > "+state+"\nif [ \"$n\" -eq 1 ]; then echo carrier auth failed >&2; exit 7; fi\ntrap 'exit 0' INT TERM\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	server, err := startServerWithRetry(context.Background(), paths{
		ServerBinary: script,
		ServerConfig: filepath.Join(dir, "server.yaml"),
		ServerLog:    filepath.Join(dir, "server.log"),
	}, 2, 0, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("startServerWithRetry() error = %v", err)
	}
	stopProcess(server)
	data, err := os.ReadFile(state)
	if err != nil {
		t.Fatalf("read attempt state: %v", err)
	}
	if strings.TrimSpace(string(data)) != "2" {
		t.Fatalf("attempt count = %q, want 2", string(data))
	}
}

func TestWaitForProbesReportsServerExit(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "server.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 0.05\necho died >&2\nexit 9\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	server, err := startServerWithGrace(context.Background(), paths{
		ServerBinary: script,
		ServerConfig: filepath.Join(dir, "server.yaml"),
		ServerLog:    filepath.Join(dir, "server.log"),
	}, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("startServerWithGrace() error = %v", err)
	}
	err = waitForProbesOrServerExit(context.Background(), server, time.Second)
	if err == nil || !strings.Contains(err.Error(), "local server exited while waiting for iOS probes") {
		t.Fatalf("waitForProbesOrServerExit() error = %v, want server exit", err)
	}
	stopProcess(server)
}

func TestPreflightResolverFallsBackAcrossEndpoints(t *testing.T) {
	var tried []string
	err := preflightResolverWithLookup(context.Background(), "127.0.0.1:1,127.0.0.1:2", "goloom.example", 1,
		func(_ context.Context, resolver string, _ string) ([]string, error) {
			tried = append(tried, resolver)
			if resolver == "127.0.0.1:1" {
				return nil, errors.New("connection reset by peer")
			}
			return []string{"192.0.2.10"}, nil
		})
	if err != nil {
		t.Fatalf("preflightResolverWithLookup() error = %v", err)
	}
	want := []string{"127.0.0.1:1", "127.0.0.1:2"}
	if strings.Join(tried, ",") != strings.Join(want, ",") {
		t.Fatalf("tried resolvers = %v, want %v", tried, want)
	}
}

func TestRetryOperationReturnsAfterTransientFailures(t *testing.T) {
	attempts := 0
	err := retryOperation(context.Background(), 3, 0, func() error {
		attempts++
		if attempts < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryOperation() error = %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}
