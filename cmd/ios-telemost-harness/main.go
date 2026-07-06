package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/iosharness"
)

const (
	defaultResolver      = "8.8.8.8:53"
	defaultProbeRounds   = 3
	defaultDownloadBytes = 1048576
	defaultProbeInterval = 8
	defaultWaitSeconds   = 240
	telemostSFUHost      = "goloom.strm.yandex.net"
)

type config struct {
	Command       string
	Workspace     string
	Stamp         string
	Resolver      string
	Device        string
	Rounds        int
	DownloadBytes int64
	ProbeInterval int
	WaitSeconds   int
	HTTPProbeURL  string
	DryRun        bool
}

type paths struct {
	ControlPlane     string
	IOSProject       string
	OlcRTCWorktree   string
	SecretRunDir     string
	ArtifactRunDir   string
	Cookies          string
	Deployment       string
	RoomStore        string
	Subscription     string
	ServerTemplate   string
	ServerConfig     string
	ServerLog        string
	ServerBinary     string
	RawIOSLogs       string
	SummaryIOSLogs   string
	ServerSummary    string
	VerdictJSON      string
	DerivedData      string
	AppBundle        string
	GenerateProfiles string
	XcodeBuildLog    string
	DevicectlLog     string
}

func main() {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := run(context.Background(), cfg, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseArgs(args []string) (config, error) {
	if len(args) == 0 {
		return config{}, errors.New("usage: ios-telemost-harness <prepare|run|summarize> [flags]")
	}
	cfg := config{
		Command:       args[0],
		Resolver:      defaultResolver,
		Rounds:        defaultProbeRounds,
		DownloadBytes: defaultDownloadBytes,
		ProbeInterval: defaultProbeInterval,
		WaitSeconds:   defaultWaitSeconds,
	}
	if cfg.Command != "prepare" && cfg.Command != "run" && cfg.Command != "summarize" {
		return config{}, fmt.Errorf("unknown command %q; want prepare, run, or summarize", cfg.Command)
	}

	fs := flag.NewFlagSet("ios-telemost-harness "+cfg.Command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.Workspace, "workspace", "", "whitelist-bypass workspace path")
	fs.StringVar(&cfg.Stamp, "stamp", "", "run stamp used for artifact paths")
	fs.StringVar(&cfg.Resolver, "resolver", defaultResolver, "DNS resolver host:port for Telemost SFU preflight and local srv config")
	fs.StringVar(&cfg.Device, "device", os.Getenv("OLC_IOS_DEVICE"), "devicectl device identifier")
	fs.IntVar(&cfg.Rounds, "rounds", defaultProbeRounds, "HTTP probe rounds")
	fs.Int64Var(&cfg.DownloadBytes, "download-bytes", defaultDownloadBytes, "download probe bytes; 0 disables bulk")
	fs.IntVar(&cfg.ProbeInterval, "probe-interval", defaultProbeInterval, "seconds between probe rounds")
	fs.IntVar(&cfg.WaitSeconds, "wait-seconds", defaultWaitSeconds, "seconds to wait before copying iOS logs")
	fs.StringVar(&cfg.HTTPProbeURL, "http-probe-url", "", "optional custom HTTP probe URL")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "print planned commands without executing")
	if err := fs.Parse(args[1:]); err != nil {
		return config{}, err
	}
	if cfg.Workspace == "" {
		workspace, err := detectWorkspace()
		if err != nil {
			return config{}, err
		}
		cfg.Workspace = workspace
	}
	if cfg.Stamp == "" {
		cfg.Stamp = time.Now().Format("20060102-150405")
	}
	if cfg.Rounds <= 0 {
		return config{}, errors.New("--rounds must be positive")
	}
	if cfg.DownloadBytes < 0 {
		return config{}, errors.New("--download-bytes cannot be negative")
	}
	if cfg.Command == "run" && cfg.Device == "" && !cfg.DryRun {
		return config{}, errors.New("--device is required for run unless --dry-run is set")
	}
	return cfg, nil
}

func detectWorkspace() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if exists(filepath.Join(dir, "control-plane", "room_manager.py")) &&
			exists(filepath.Join(dir, "client", "ios", "OlcClientiOS")) {
			return dir, nil
		}
		next := filepath.Dir(dir)
		if next == dir {
			break
		}
	}
	return "", errors.New("--workspace is required; could not detect whitelist-bypass workspace")
}

func (cfg config) paths() paths {
	secretRun := filepath.Join(cfg.Workspace, ".secrets", "runtime", "harness", cfg.Stamp)
	artifactRun := filepath.Join(cfg.Workspace, "artifacts", "telemost-fix", "harness", cfg.Stamp)
	iosProject := filepath.Join(cfg.Workspace, "client", "ios", "OlcClientiOS")
	derivedData := filepath.Join(artifactRun, "DerivedData")
	return paths{
		ControlPlane:     filepath.Join(cfg.Workspace, "control-plane"),
		IOSProject:       iosProject,
		OlcRTCWorktree:   detectOlcRTCWorktree(cfg.Workspace),
		SecretRunDir:     secretRun,
		ArtifactRunDir:   artifactRun,
		Cookies:          filepath.Join(cfg.Workspace, ".secrets", "telemost-account", "cookie-header.txt"),
		Deployment:       filepath.Join(cfg.Workspace, ".secrets", "olc-stand", "deployment.json"),
		RoomStore:        filepath.Join(secretRun, "rooms.json"),
		Subscription:     filepath.Join(secretRun, "telemost-subscription.json"),
		ServerTemplate:   filepath.Join(cfg.Workspace, ".secrets", "runtime", "olc-srv-direct.yaml"),
		ServerConfig:     filepath.Join(secretRun, "olc-srv.yaml"),
		ServerLog:        filepath.Join(secretRun, "logs", "server.log"),
		ServerBinary:     filepath.Join(cfg.Workspace, "artifacts", "telemost-fix", "bin", "olcrtc-fix-telemost-darwin"),
		RawIOSLogs:       filepath.Join(secretRun, "ios-logs"),
		SummaryIOSLogs:   filepath.Join(artifactRun, "ios-logs"),
		ServerSummary:    filepath.Join(artifactRun, "logs", "server-summary.log"),
		VerdictJSON:      filepath.Join(artifactRun, "verdict.json"),
		DerivedData:      derivedData,
		AppBundle:        filepath.Join(derivedData, "Build", "Products", "Debug-iphoneos", "OlcClientiOS.app"),
		GenerateProfiles: filepath.Join(iosProject, "scripts", "generate-local-profiles.rb"),
		XcodeBuildLog:    filepath.Join(artifactRun, "logs", "xcodebuild.log"),
		DevicectlLog:     filepath.Join(artifactRun, "logs", "devicectl.log"),
	}
}

func detectOlcRTCWorktree(workspace string) string {
	wt := filepath.Join(workspace, "olcrtc-fork", ".worktrees", "fix-telemost")
	if exists(filepath.Join(wt, "go.mod")) {
		return wt
	}
	return filepath.Join(workspace, "olcrtc-fork")
}

func run(ctx context.Context, cfg config, stdout io.Writer, stderr io.Writer) error {
	p := cfg.paths()
	if cfg.DryRun {
		return printPlan(cfg, p, stdout)
	}
	switch cfg.Command {
	case "prepare":
		return prepare(ctx, cfg, p, stdout, stderr)
	case "summarize":
		_, err := summarize(cfg, p, time.Time{}, stdout)
		return err
	case "run":
		if err := prepare(ctx, cfg, p, stdout, stderr); err != nil {
			return err
		}
		return runProbe(ctx, cfg, p, stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", cfg.Command)
	}
}

func printPlan(cfg config, p paths, stdout io.Writer) error {
	plan := map[string]any{
		"command":        cfg.Command,
		"workspace":      cfg.Workspace,
		"stamp":          cfg.Stamp,
		"secret_run_dir": p.SecretRunDir,
		"artifact_dir":   p.ArtifactRunDir,
		"resolver":       cfg.Resolver,
		"rounds":         cfg.Rounds,
		"download_bytes": cfg.DownloadBytes,
		"device":         cfg.Device,
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(plan)
}

func prepare(ctx context.Context, cfg config, p paths, stdout io.Writer, stderr io.Writer) error {
	for _, dir := range []string{p.SecretRunDir, filepath.Join(p.SecretRunDir, "logs"), p.ArtifactRunDir, filepath.Dir(p.XcodeBuildLog)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if err := preflightResolver(ctx, cfg.Resolver, telemostSFUHost); err != nil {
		return err
	}
	if err := runRoomManager(ctx, p, stderr); err != nil {
		return err
	}
	sub, err := readSubscription(p.Subscription)
	if err != nil {
		return err
	}
	if err := iosharness.WriteServerConfig(p.ServerTemplate, p.ServerConfig, sub, cfg.Resolver, true); err != nil {
		return err
	}
	if err := generateIOSProfiles(ctx, p, stderr); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "prepared fresh Telemost harness run %s\n", cfg.Stamp)
	fmt.Fprintf(stdout, "secret run dir: %s\n", p.SecretRunDir)
	fmt.Fprintf(stdout, "artifact dir: %s\n", p.ArtifactRunDir)
	return nil
}

func runRoomManager(ctx context.Context, p paths, stderr io.Writer) error {
	out, err := os.Create(p.Subscription) // #nosec G304 -- explicit local harness path
	if err != nil {
		return err
	}
	defer out.Close()
	cmd := exec.CommandContext(ctx, "python3", "room_manager.py",
		"--cookies", p.Cookies,
		"--deployment", p.Deployment,
		"--store", p.RoomStore,
		"subscription",
	)
	cmd.Dir = p.ControlPlane
	cmd.Env = roomManagerEnv(os.Environ(), certifiBundle(ctx))
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("room_manager subscription: %w", err)
	}
	return nil
}

func roomManagerEnv(base []string, certFile string) []string {
	if certFile == "" || hasEnv(base, "SSL_CERT_FILE") || hasEnv(base, "SSL_CERT_DIR") {
		return base
	}
	return append(base, "SSL_CERT_FILE="+certFile)
}

func hasEnv(env []string, key string) bool {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}

func certifiBundle(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, "python3", "-c", "import certifi; print(certifi.where())")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(string(out))
	if path == "" || !exists(path) {
		return ""
	}
	return path
}

func readSubscription(path string) (iosharness.Subscription, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- explicit local harness path
	if err != nil {
		return iosharness.Subscription{}, err
	}
	var sub iosharness.Subscription
	if err := json.Unmarshal(data, &sub); err != nil {
		return iosharness.Subscription{}, err
	}
	return sub, nil
}

func generateIOSProfiles(ctx context.Context, p paths, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "ruby", p.GenerateProfiles, "--allow-missing-wb")
	cmd.Dir = p.IOSProject
	cmd.Env = append(os.Environ(),
		"RUBYOPT=--disable=gems",
		"OLC_TELEMOST_SUBSCRIPTION_JSON="+p.Subscription,
	)
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("generate iOS local profiles: %w", err)
	}
	return nil
}

func runProbe(ctx context.Context, cfg config, p paths, stdout io.Writer, stderr io.Writer) error {
	if err := ensureServerBinary(ctx, p, stderr); err != nil {
		return err
	}
	if err := buildIOS(ctx, p); err != nil {
		return err
	}
	server, err := startServer(ctx, p)
	if err != nil {
		return err
	}
	defer stopProcess(server)

	probeSince := time.Now().Add(-2 * time.Second)
	if err := installAndLaunch(ctx, cfg, p); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "waiting %ds for iOS probes...\n", cfg.WaitSeconds)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(cfg.WaitSeconds) * time.Second):
	}
	if err := copyIOSLogs(ctx, cfg, p); err != nil {
		return err
	}
	verdict, err := summarize(cfg, p, probeSince, stdout)
	if err != nil {
		return err
	}
	if !verdict.Green {
		return fmt.Errorf("probe verdict red: %s", strings.Join(verdict.Reasons, "; "))
	}
	return nil
}

func ensureServerBinary(ctx context.Context, p paths, stderr io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(p.ServerBinary), 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-o", p.ServerBinary, "./cmd/olcrtc")
	cmd.Dir = p.OlcRTCWorktree
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build olcrtc server binary: %w", err)
	}
	return nil
}

func buildIOS(ctx context.Context, p paths) error {
	logFile, err := os.Create(p.XcodeBuildLog) // #nosec G304 -- explicit local harness path
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.CommandContext(ctx, "xcodebuild", xcodeBuildArgs(p)...)
	cmd.Dir = p.IOSProject
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("xcodebuild iOS app: %w (log: %s)", err, p.XcodeBuildLog)
	}
	return nil
}

func xcodeBuildArgs(p paths) []string {
	return []string{
		"-project", "OlcClientiOS.xcodeproj",
		"-scheme", "OlcClientiOS",
		"-configuration", "Debug",
		"-destination", "generic/platform=iOS",
		"-derivedDataPath", p.DerivedData,
		"OTHER_LDFLAGS=-lresolv",
		"build",
	}
}

type serverProcess struct {
	cmd     *exec.Cmd
	logFile *os.File
}

func startServer(ctx context.Context, p paths) (*serverProcess, error) {
	logFile, err := os.Create(p.ServerLog) // #nosec G304 -- explicit local harness path
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, p.ServerBinary, p.ServerConfig)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start local server: %w", err)
	}
	time.Sleep(3 * time.Second)
	return &serverProcess{cmd: cmd, logFile: logFile}, nil
}

func stopProcess(server *serverProcess) {
	if server == nil || server.cmd == nil || server.cmd.Process == nil {
		return
	}
	_ = server.cmd.Process.Signal(os.Interrupt)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	done := make(chan struct{})
	go func() {
		_ = server.cmd.Wait()
		_ = server.logFile.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-timer.C:
		_ = server.cmd.Process.Kill()
		_ = server.logFile.Close()
	}
}

func installAndLaunch(ctx context.Context, cfg config, p paths) error {
	logFile, err := os.OpenFile(p.DevicectlLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304
	if err != nil {
		return err
	}
	defer logFile.Close()
	install := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "install", "app",
		"--device", cfg.Device,
		p.AppBundle,
	)
	install.Stdout = logFile
	install.Stderr = logFile
	if err := install.Run(); err != nil {
		return fmt.Errorf("install iOS app: %w (log: %s)", err, p.DevicectlLog)
	}

	args := []string{
		"devicectl", "device", "process", "launch", "--terminate-existing",
		"--device", cfg.Device,
		"ru.unite.olc.ios",
		"--profile-id", "telemost",
		"--connect-on-launch",
		"--probe-rounds", strconv.Itoa(cfg.Rounds),
		"--probe-interval", strconv.Itoa(cfg.ProbeInterval),
		"--probe-download-bytes", strconv.FormatInt(cfg.DownloadBytes, 10),
	}
	if cfg.HTTPProbeURL != "" {
		args = append(args, "--http-probe-url", cfg.HTTPProbeURL)
	}
	launch := exec.CommandContext(ctx, "xcrun", args...)
	launch.Stdout = logFile
	launch.Stderr = logFile
	if err := launch.Run(); err != nil {
		return fmt.Errorf("launch iOS app: %w (log: %s)", err, p.DevicectlLog)
	}
	return nil
}

func copyIOSLogs(ctx context.Context, cfg config, p paths) error {
	if err := os.MkdirAll(p.RawIOSLogs, 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "copy", "from",
		"--device", cfg.Device,
		"--domain-type", "appGroupDataContainer",
		"--domain-identifier", "group.ru.unite.olc",
		"--source", "olc",
		"--destination", p.RawIOSLogs,
		"--remove-existing-content", "true",
	)
	logFile, err := os.OpenFile(p.DevicectlLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("copy iOS logs: %w (log: %s)", err, p.DevicectlLog)
	}
	return nil
}

func summarize(cfg config, p paths, since time.Time, stdout io.Writer) (iosharness.Verdict, error) {
	verdict, err := iosharness.SummarizeLogs(
		iosharness.RawLogPaths{IOSDir: p.RawIOSLogs, ServerLog: p.ServerLog},
		iosharness.SummaryPaths{IOSDir: p.SummaryIOSLogs, ServerSummary: p.ServerSummary},
		iosharness.Criteria{Rounds: cfg.Rounds, DownloadBytes: cfg.DownloadBytes, Since: since},
	)
	if err != nil {
		return iosharness.Verdict{}, err
	}
	if err := os.MkdirAll(filepath.Dir(p.VerdictJSON), 0o700); err != nil {
		return iosharness.Verdict{}, err
	}
	data, err := json.MarshalIndent(verdict, "", "  ")
	if err != nil {
		return iosharness.Verdict{}, err
	}
	if err := os.WriteFile(p.VerdictJSON, append(data, '\n'), 0o600); err != nil {
		return iosharness.Verdict{}, err
	}
	fmt.Fprintf(stdout, "verdict green=%v reasons=%v\n", verdict.Green, verdict.Reasons)
	fmt.Fprintf(stdout, "summaries: %s %s\n", p.SummaryIOSLogs, p.ServerSummary)
	return verdict, nil
}

func preflightResolver(ctx context.Context, resolver string, host string) error {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, "udp", resolver)
		},
	}
	ips, err := r.LookupHost(ctx, host)
	if err != nil {
		return fmt.Errorf("DNS preflight failed for %s via %s: %w", host, resolver, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("DNS preflight returned no addresses for %s via %s", host, resolver)
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
