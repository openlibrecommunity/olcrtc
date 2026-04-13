//go:build mage

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

const (
	module    = "github.com/openlibrecommunity/olcrtc"
	buildDir  = "build"
	ldflags   = "-s -w"
	goVersion = "1.24"
)

var (
	goexe  = mg.GoCmd()
	goos   = envOr("GOOS", runtime.GOOS)
	goarch = envOr("GOARCH", runtime.GOARCH)
)

// Build builds both olcrtc CLI and UI binaries.
func Build() error {
	mg.Deps(BuildCLI, BuildUI)
	return nil
}

// BuildCLI builds the olcrtc server/client binary.
func BuildCLI() error {
	mg.Deps(Deps)
	return buildBinary("olcrtc", "./cmd/olcrtc", goos, goarch)
}

// BuildUI builds the Fyne desktop UI binary.
func BuildUI() error {
	return buildUIBinary(goos, goarch)
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

	return nil
}

// Podman builds the image using podman.
func Podman() error {
	tag := envOr("DOCKER_TAG", "olcrtc:latest")
	return sh.RunV("podman", "build", "-t", tag, ".")
}

// Docker builds the image using docker.
func Docker() error {
	tag := envOr("DOCKER_TAG", "olcrtc:latest")
	return sh.RunV("docker", "build", "-t", tag, ".")
}

// Lint runs golangci-lint.
func Lint() error {
	if err := ensureTool("golangci-lint"); err != nil {
		return fmt.Errorf("golangci-lint not found, install it:\n  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest")
	}
	return sh.RunV("golangci-lint", "run", "./...")
}

// Test runs all tests.
func Test() error {
	return sh.RunV(goexe, "test", "-race", "-count=1", "./...")
}

// Deps downloads and tidies Go module dependencies.
func Deps() error {
	if err := sh.RunV(goexe, "mod", "download"); err != nil {
		return err
	}
	return sh.RunV(goexe, "mod", "tidy")
}

// Clean removes build artifacts.
func Clean() error {
	return os.RemoveAll(buildDir)
}

// Mobile builds the Android AAR via gomobile.
func Mobile() error {
	if err := ensureTool("gomobile"); err != nil {
		return fmt.Errorf("gomobile not found: run 'go install golang.org/x/mobile/cmd/gomobile@latest && gomobile init'")
	}
	return sh.RunV("gomobile", "bind",
		"-target=android",
		"-androidapi", "21",
		"-o", filepath.Join(buildDir, "olcrtc.aar"),
		"./mobile",
	)
}

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

	return sh.RunWithV(env, goexe, "build",
		"-trimpath",
		"-ldflags", flags,
		"-o", out,
		pkg,
	)
}

func buildUIBinary(os_, arch string) error {
	if err := ensureBuildDir(); err != nil {
		return err
	}

	ext := ""
	if os_ == "windows" {
		ext = ".exe"
	}
	outName := fmt.Sprintf("%s-%s-%s%s", "olcrtc-ui", os_, arch, ext)
	absOut, err := filepath.Abs(filepath.Join(buildDir, outName))
	if err != nil {
		return err
	}

	fmt.Printf("building olcrtc-ui (%s/%s)\n", os_, arch)

	cmd := exec.Command(goexe, "build",
		"-trimpath",
		"-ldflags", ldflags,
		"-o", absOut,
		".",
	)
	cmd.Dir = "ui"
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"GOOS="+os_,
		"GOARCH="+arch,
	)

	return cmd.Run()
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
