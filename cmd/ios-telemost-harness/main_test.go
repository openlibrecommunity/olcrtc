package main

import (
	"testing"
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
