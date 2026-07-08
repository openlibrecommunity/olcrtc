//go:build mage

// Magefile for olcrtc.
//
// Quick reference:
//
//	mage check          # build + vet + lint + secrets + unit tests (pre-commit)
//	mage all            # full pre-merge pipeline (check + e2e smoke matrix)
//	mage nightly        # everything including stress matrix (~6h)
//
//	mage build          # native binary
//	mage cross          # all platforms
//	mage mobile         # Android AAR
//
//	mage test           # short unit tests
//	mage testfull       # all unit tests, no real providers
//	mage e2e            # real-provider smoke matrix
//	mage stress         # real-provider stress matrix (long)
//	mage soak           # real-provider throughput soak (long)
//	mage localsoak      # local in-memory soak (very long, no network)
//
// Tunables (env):
//
//	E2E_CARRIERS, E2E_TRANSPORTS, E2E_TIMEOUT
//	E2E_STRESS, E2E_STRESS_DURATION
//	STRESS_BULK_DURATION, STRESS_ECHO_DURATION, STRESS_CASE_TIMEOUT, STRESS_TIMEOUT
//	SOAK_CARRIERS, SOAK_TRANSPORTS, SOAK_DURATION, SOAK_CHAOS

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
)

// Default target when invoked as `mage` with no args.
//
//nolint:gochecknoglobals // mage requires a package-level Default symbol.
var Default = Help

const (
	buildDir = "build"
	ldflags  = "-s -w"
)

var (
	goexe  = mg.GoCmd()
	goos   = envOr("GOOS", runtime.GOOS)
	goarch = envOr("GOARCH", runtime.GOARCH)
)

// ─────────────────────────────────────────────────────────────────────────────
// Pipelines
// ─────────────────────────────────────────────────────────────────────────────

// Help lists every target in mage's default style. This is what runs when
// you invoke `mage` with no arguments. For grouped/annotated docs see the
// magefile header.
func Help() error {
	return sh.RunV("mage", "-l")
}

// Check runs the fast pre-commit pipeline: build + vet + lint + secrets + unit tests.
// Use this before every commit.
func Check() {
	mg.SerialDeps(Build, Vet, Lint, Secrets, TestFull)
}

// All runs the full pre-merge pipeline: Check + the real-provider smoke
// matrix. Stress and soak are NOT included - run them explicitly when
// needed (see Nightly for everything).
func All() {
	mg.SerialDeps(Check, E2e)
}

// Nightly runs everything: All + stress matrix. Expect multi-hour runtime;
// intended for the nightly CI job or manual deep validation.
func Nightly() {
	mg.SerialDeps(All, Stress)
}

// Everything runs literally every test stage: Nightly + real soak + local
// soak. Expect 12+ hours; this is for "I want maximum confidence before
// shipping" runs, not regular development. Tune SOAK_DURATION etc. if you
// need a shorter window.
func Everything() {
	mg.SerialDeps(Nightly, Soak, LocalSoak)
}

// ─────────────────────────────────────────────────────────────────────────────
// Build
// ─────────────────────────────────────────────────────────────────────────────

// Build builds the olcrtc CLI binary for the host platform.
func Build() error {
	mg.Deps(Deps)
	return buildBinary("olcrtc", "./cmd/olcrtc", goos, goarch)
}

// Cross builds olcrtc for all supported platforms.
func Cross() error {
	mg.Deps(Deps)

	targets := []struct{ os, arch string }{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"windows", "amd64"},
		{"darwin", "amd64"},
		{"darwin", "arm64"},
		{"freebsd", "amd64"},
		{"freebsd", "arm64"},
		{"openbsd", "amd64"},
		{"openbsd", "arm64"},
	}

	for _, t := range targets {
		if err := buildBinary("olcrtc", "./cmd/olcrtc", t.os, t.arch); err != nil {
			return err
		}
	}

	fmt.Printf("✅ built %d platform(s)\n", len(targets))
	return nil
}

// Mobile builds the Android AAR via gomobile.
func Mobile() error {
	if err := ensureTool("gomobile"); err != nil {
		return fmt.Errorf("gomobile not found: run 'go install golang.org/x/mobile/cmd/gomobile@latest && gomobile init'")
	}
	if err := ensureBuildDir(); err != nil {
		return err
	}
	return sh.RunV("gomobile", "bind",
		"-target=android",
		"-androidapi", "21",
		"-ldflags", "-s -w -checklinkname=0",
		"-o", filepath.Join(buildDir, "olcrtc.aar"),
		"./mobile",
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Quality
// ─────────────────────────────────────────────────────────────────────────────

// Vet runs go vet on the whole module.
func Vet() error {
	return sh.RunV(goexe, "vet", "./...")
}

// Lint runs golangci-lint.
func Lint() error {
	if err := ensureTool("golangci-lint"); err != nil {
		return fmt.Errorf("golangci-lint not found, install it:\n  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest")
	}
	return sh.RunV("golangci-lint", "run", "./...")
}

// Secrets scans the repository for committed secrets, local paths and runtime artifact references.
func Secrets() error {
	return sh.RunV("script/secrets-check.sh")
}

// Tidy runs go mod tidy and verifies modules.
func Tidy() error {
	if err := sh.RunV(goexe, "mod", "tidy"); err != nil {
		return err
	}
	return sh.RunV(goexe, "mod", "verify")
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// Test runs unit tests in -short mode (skips long-running tests).
func Test() error {
	return sh.RunV(goexe, "test", "-race", "-count=1", "-short", "./...")
}

// TestFull runs all unit + fast e2e tests with race detector. No real providers.
func TestFull() error {
	return sh.RunV(goexe, "test", "-race", "-count=1", "-timeout", "10m", "./...")
}

// E2e runs the real-provider smoke matrix.
// Configure via env: E2E_CARRIERS, E2E_TRANSPORTS, E2E_TIMEOUT, E2E_STRESS.
//
// Note: -race is intentionally NOT enabled here. The race detector adds
// significant CPU overhead and breaks timing-sensitive handshake tests
// against real networks (telemost/videochannel handshake regularly times
// out under -race). Race coverage for production code paths is provided
// by `mage testFull` against in-memory carriers.
func E2e() error {
	args := []string{"test", "-count=1", "-v", "-timeout", "30m",
		"./internal/e2e/...",
		"-olcrtc.real-e2e=true",
	}
	if carriers := os.Getenv("E2E_CARRIERS"); carriers != "" {
		args = append(args, "-olcrtc.real-carriers="+carriers)
	}
	if transports := os.Getenv("E2E_TRANSPORTS"); transports != "" {
		args = append(args, "-olcrtc.real-transports="+transports)
	}
	if timeout := os.Getenv("E2E_TIMEOUT"); timeout != "" {
		args = append(args, "-olcrtc.real-timeout="+timeout)
	}
	if os.Getenv("E2E_STRESS") != "" {
		args = append(args, "-olcrtc.stress=true")
		if d := os.Getenv("E2E_STRESS_DURATION"); d != "" {
			args = append(args, "-olcrtc.stress-duration="+d)
		}
	}
	return sh.RunV(goexe, args...)
}

// Stress runs the real-provider stress matrix on every carrier × transport pair.
// Defaults match the long nightly profile (15m bulk + 15m echo, 35m hard cap per case).
// Override via env: STRESS_BULK_DURATION, STRESS_ECHO_DURATION, STRESS_CASE_TIMEOUT,
// STRESS_TIMEOUT, E2E_CARRIERS, E2E_TRANSPORTS.
func Stress() error {
	bulk := envOr("STRESS_BULK_DURATION", "15m")
	echo := envOr("STRESS_ECHO_DURATION", "15m")
	caseTO := envOr("STRESS_CASE_TIMEOUT", "35m")
	overall := envOr("STRESS_TIMEOUT", "6h")

	args := []string{"test", "-count=1", "-v",
		"-timeout", overall,
		"-run", "^TestRealProviderTransportStress$",
		"./internal/e2e/...",
		"-olcrtc.real-e2e=true",
		"-olcrtc.stress=true",
		"-olcrtc.stress-bulk-duration=" + bulk,
		"-olcrtc.stress-duration=" + echo,
		"-olcrtc.stress-case-timeout=" + caseTO,
	}
	if carriers := os.Getenv("E2E_CARRIERS"); carriers != "" {
		args = append(args, "-olcrtc.real-carriers="+carriers)
	}
	if transports := os.Getenv("E2E_TRANSPORTS"); transports != "" {
		args = append(args, "-olcrtc.real-transports="+transports)
	}
	return sh.RunV(goexe, args...)
}

// Soak runs the real-provider throughput soak test.
// Configure via env: SOAK_CARRIERS, SOAK_TRANSPORTS, SOAK_DURATION.
func Soak() error {
	carriers := envOr("SOAK_CARRIERS", "telemost,jitsi,wbstream")
	transports := envOr("SOAK_TRANSPORTS", "datachannel,vp8channel")
	duration := envOr("SOAK_DURATION", "10m")

	args := []string{"test", "-count=1", "-v",
		"-timeout", "12h",
		"-run", "^TestRealThroughputSoak$",
		"./internal/e2e/...",
		"-olcrtc.real-e2e=true",
		"-olcrtc.real-soak=true",
		"-olcrtc.real-soak-carrier=" + carriers,
		"-olcrtc.real-soak-transport=" + transports,
		"-olcrtc.real-soak-duration=" + duration,
	}
	return sh.RunV(goexe, args...)
}

// LocalSoak runs the local (in-memory) throughput soak.
// Configure via env: SOAK_TRANSPORTS, SOAK_DURATION, SOAK_CHAOS.
func LocalSoak() error {
	transports := envOr("SOAK_TRANSPORTS", "all")
	duration := envOr("SOAK_DURATION", "6m")
	chaos := os.Getenv("SOAK_CHAOS")

	args := []string{"test", "-count=1", "-v",
		"-timeout", "12h",
		"-run", "^TestLocalThroughputSoak$",
		"./internal/e2e/...",
		"-olcrtc.local-soak=true",
		"-olcrtc.local-soak-transport=" + transports,
		"-olcrtc.local-soak-duration=" + duration,
	}
	if chaos != "" {
		args = append(args, "-olcrtc.local-soak-chaos="+chaos)
	}
	return sh.RunV(goexe, args...)
}

// ─────────────────────────────────────────────────────────────────────────────
// Utility
// ─────────────────────────────────────────────────────────────────────────────

// Deps downloads Go module dependencies.
func Deps() error {
	return sh.RunV(goexe, "mod", "download")
}

// Clean removes build artifacts.
func Clean() error {
	return os.RemoveAll(buildDir)
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers (unexported, not visible as targets)
// ─────────────────────────────────────────────────────────────────────────────

func buildBinary(name, pkg, os_, arch string) error {
	if err := ensureBuildDir(); err != nil {
		return err
	}

	ext := ""
	if os_ == "windows" {
		ext = ".exe"
	}
	outName := fmt.Sprintf("%s-%s-%s%s", name, os_, arch, ext)
	out := filepath.Join(buildDir, outName)
	fmt.Printf("building %s (%s/%s) -> %s\n", name, os_, arch, out)

	env := map[string]string{
		"GOOS":        os_,
		"GOARCH":      arch,
		"CGO_ENABLED": "0",
	}

	flags := ldflags
	if os_ == "android" {
		flags += " -checklinkname=0"
	}

	args := []string{"build", "-trimpath", "-ldflags", flags, "-o", out, pkg}
	return sh.RunWithV(env, goexe, args...)
}

func ensureBuildDir() error {
	return os.MkdirAll(buildDir, 0o755)
}

func ensureTool(name string) error {
	_, err := exec.LookPath(name)
	return err
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
