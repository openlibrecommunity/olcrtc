// Package main provides the olcrtc CLI entrypoint.
//
// Usage: olcrtc <config.yaml>
//
// All runtime settings come from the YAML file. There are no other CLI flags.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	protoLogger "github.com/livekit/protocol/logger"
	lksdk "github.com/owenewans/owenlivekit/v2"

	"github.com/openlibrecommunity/olcrtc/internal/app/session"
	configpkg "github.com/openlibrecommunity/olcrtc/internal/config"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/names"
	"github.com/openlibrecommunity/olcrtc/internal/supervisor"
)

// shutdownGrace bounds how long a cancelled session may take to unwind.
const shutdownGrace = 5 * time.Second

// ErrConfigPathRequired is returned when no config file is provided.
var ErrConfigPathRequired = errors.New("usage: olcrtc <config.yaml>")

// ErrProfilesUnsupportedForGen is returned when failover profiles are configured for gen mode.
var ErrProfilesUnsupportedForGen = errors.New("profiles are only supported for srv and cnc modes")

//nolint:gochecknoglobals // Tests replace the long-running session runner with a bounded function.
var runSession = session.Run

//nolint:gochecknoglobals // Tests replace gen runner with a stub.
var runGen = execGen

// loadedConfig bundles the parsed YAML file and the derived session config.
type loadedConfig struct {
	scfg     session.Config
	profiles []supervisor.Profile
	failover failoverConfig
	dataDir  string
	debug    bool
	// reload re-reads the config file for the current failover profiles. It lets
	// the supervisor pick up rooms added while a session was live (dynamic list).
	reload func() ([]supervisor.Profile, error)
}

type failoverConfig struct {
	retryDelay time.Duration
	maxCycles  int
}

func main() {
	err := run()

	// Report before the filter is torn down: flushStderrFilter restores the
	// real stderr, but the error still has to travel through the filter
	// goroutine to stay ordered with everything already buffered in it.
	if err != nil {
		logger.Error(err)
	}

	flushStderrFilter()

	if err != nil {
		os.Exit(1)
	}
}

func run() error {
	return runWithArgs(os.Args[1:])
}

func runWithArgs(args []string) error {
	installStderrFilter()
	session.RegisterDefaults()

	if len(args) != 1 || args[0] == "-h" || args[0] == "--help" || args[0] == "-help" {
		return ErrConfigPathRequired
	}

	cfg, err := loadConfig(args[0])
	if err != nil {
		return err
	}

	return runWithConfig(cfg)
}

func loadConfig(path string) (loadedConfig, error) {
	file, err := configpkg.Load(path)
	if err != nil {
		return loadedConfig{}, fmt.Errorf("load config: %w", err)
	}

	base := configpkg.Apply(file)

	profiles := make([]supervisor.Profile, 0, len(file.Profiles))
	for i, profile := range file.Profiles {
		name := profile.Name
		if name == "" {
			name = fmt.Sprintf("profile-%d", i+1)
		}

		profiles = append(profiles, supervisor.Profile{
			Name:   name,
			Config: configpkg.ApplyProfile(base, profile),
		})
	}

	failover, err := parseFailoverConfig(file.Failover)
	if err != nil {
		return loadedConfig{}, err
	}

	return loadedConfig{
		scfg:     base,
		profiles: profiles,
		failover: failover,
		dataDir:  resolveDataDir(path, file.Data),
		debug:    file.Debug,
		reload:   func() ([]supervisor.Profile, error) { return loadProfiles(path) },
	}, nil
}

// loadProfiles re-reads the config file and returns the currently configured
// failover profiles, applied and validated. Invalid profiles are skipped so one
// bad entry does not stall a reload. Used as the supervisor's dynamic Reload
// hook: rooms written to the config while a session was live are picked up the
// instant that session ends, without restarting the process.
func loadProfiles(path string) ([]supervisor.Profile, error) {
	file, err := configpkg.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	base := configpkg.Apply(file)

	profiles := make([]supervisor.Profile, 0, len(file.Profiles))
	for i, profile := range file.Profiles {
		name := profile.Name
		if name == "" {
			name = fmt.Sprintf("profile-%d", i+1)
		}

		scfg := session.ApplyDefaults(configpkg.ApplyProfile(base, profile))
		if err := session.Validate(scfg); err != nil {
			logger.Warnf("skipping invalid failover profile %q on reload: %v", name, err)

			continue
		}

		profiles = append(profiles, supervisor.Profile{Name: name, Config: scfg})
	}

	return profiles, nil
}

func parseFailoverConfig(f configpkg.Failover) (failoverConfig, error) {
	retryDelay := supervisor.DefaultRetryDelay

	if f.RetryDelay != "" {
		parsed, err := time.ParseDuration(f.RetryDelay)
		if err != nil {
			return failoverConfig{}, fmt.Errorf("parse failover.retry_delay: %w", err)
		}

		retryDelay = parsed
	}

	return failoverConfig{retryDelay: retryDelay, maxCycles: f.MaxCycles}, nil
}

func runWithConfig(cfg loadedConfig) error {
	configureLogging(cfg.debug)

	scfg := session.ApplyDefaults(cfg.scfg)

	if scfg.Mode == session.ModeGen {
		if len(cfg.profiles) > 0 {
			return ErrProfilesUnsupportedForGen
		}

		return runGen(scfg)
	}

	if len(cfg.profiles) > 0 {
		profiles := prepareProfiles(cfg.profiles)
		return runFailoverSessionMode(cfg.dataDir, profiles, cfg.failover, cfg.reload)
	}

	return runSessionMode(cfg.dataDir, scfg)
}

func prepareProfiles(profiles []supervisor.Profile) []supervisor.Profile {
	out := make([]supervisor.Profile, 0, len(profiles))

	for _, profile := range profiles {
		profile.Config = session.ApplyDefaults(profile.Config)
		out = append(out, profile)
	}

	return out
}

func runSessionMode(dataDir string, scfg session.Config) error {
	if err := session.Validate(scfg); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	if err := loadNameOverrides(dataDir); err != nil {
		return err
	}

	return runManaged(func(ctx context.Context) error {
		return runSession(ctx, scfg)
	})
}

func runFailoverSessionMode(
	dataDir string,
	profiles []supervisor.Profile,
	failover failoverConfig,
	reload func() ([]supervisor.Profile, error),
) error {
	for _, profile := range profiles {
		if err := session.Validate(profile.Config); err != nil {
			return fmt.Errorf("validate profile %q: %w", profile.Name, err)
		}
	}

	if err := loadNameOverrides(dataDir); err != nil {
		return err
	}

	return runManaged(func(ctx context.Context) error {
		return supervisor.Run(ctx, supervisor.Config{
			Profiles:   profiles,
			Reload:     reload,
			RetryDelay: failover.retryDelay,
			MaxCycles:  failover.maxCycles,
			OnProfileStart: func(profile supervisor.Profile, cycle int) {
				logger.Infof("failover cycle=%d starting profile=%s provider=%s transport=%s",
					cycle, profile.Name, profile.Config.Provider, profile.Config.Transport)
			},
			OnProfileEnd: func(profile supervisor.Profile, cycle int, err error) {
				if err != nil {
					logger.Warnf("failover cycle=%d profile=%s ended with error: %v", cycle, profile.Name, err)

					return
				}

				logger.Warnf("failover cycle=%d profile=%s ended", cycle, profile.Name)
			},
			OnStatus: logFailoverStatus,
		}, runSession)
	})
}

func logFailoverStatus(status supervisor.Status) {
	if !logger.IsVerbose() {
		return
	}

	active := status.ActiveProfile
	if active == "" {
		active = "none"
	}

	logger.Debugf("failover status cycle=%d active=%s last_error=%q profiles=%s history=%d",
		status.Cycle, active, status.LastError, formatProfileStatuses(status.Profiles), len(status.History))
}

func formatProfileStatuses(profiles []supervisor.ProfileStatus) string {
	if len(profiles) == 0 {
		return "[]"
	}

	var buf bytes.Buffer

	_ = buf.WriteByte('[')

	for i, profile := range profiles {
		if i > 0 {
			_ = buf.WriteByte(' ')
		}

		_, _ = fmt.Fprintf(&buf, "%s{starts=%d failures=%d clean=%d}",
			profile.Name, profile.Starts, profile.Failures, profile.CleanEnds)
	}

	_ = buf.WriteByte(']')

	return buf.String()
}

func runManaged(run func(context.Context) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	defer signal.Stop(sigCh)

	errCh := make(chan error, 1)

	go func() {
		errCh <- run(ctx)
	}()

	select {
	case <-sigCh:
		logger.Info("Shutting down gracefully...")
		cancel()

		return waitForShutdown(errCh)
	case err := <-errCh:
		return err
	}
}

func execGen(scfg session.Config) error {
	if err := session.ValidateGen(scfg); err != nil {
		return fmt.Errorf("validate gen config: %w", err)
	}

	return runManaged(func(ctx context.Context) error {
		return session.Gen(ctx, scfg, func(id string) { _, _ = fmt.Fprintln(os.Stdout, id) })
	})
}

// noisyPrefixes lists log fragments from third-party libs that spam via std log.
var noisyPrefixes = [][]byte{ //nolint:gochecknoglobals // package-level filter list
	[]byte("turnc"), []byte("[turn]"), []byte("Fail to refresh permissions"),
}

// filteredWriter wraps an io.Writer and drops lines matching noisyPrefixes.
type filteredWriter struct{ w io.Writer }

func (f filteredWriter) Write(p []byte) (int, error) {
	if isNoisyLogLine(p) {
		return len(p), nil
	}

	n, err := f.w.Write(p)
	if err != nil {
		return n, fmt.Errorf("log write: %w", err)
	}

	return n, nil
}

func isNoisyLogLine(line []byte) bool {
	for _, prefix := range noisyPrefixes {
		if bytes.Contains(line, prefix) {
			return true
		}
	}

	return false
}

func configureLogging(debug bool) {
	log.SetOutput(filteredWriter{w: os.Stderr})
	logger.DisableNoisyPionLogs()

	if debug {
		logger.SetVerbose(true)

		return
	}

	_ = os.Setenv("PION_LOG_DISABLE", "all")
	lksdk.SetLogger(protoLogger.GetDiscardLogger())
}

// resolveDataDir resolves the optional `data:` override relative to the config
// file, matching how `crypto.key_file` is resolved. An empty value means "use
// the dictionaries embedded in the binary".
func resolveDataDir(configPath, dataDir string) string {
	if dataDir == "" || filepath.IsAbs(dataDir) {
		return dataDir
	}

	return filepath.Join(filepath.Dir(configPath), dataDir)
}

// loadNameOverrides swaps the embedded display-name dictionaries for the ones
// in dataDir. Nothing to do when the operator did not ask for an override.
func loadNameOverrides(dataDir string) error {
	if dataDir == "" {
		return nil
	}

	namesPath := filepath.Join(dataDir, "names")
	surnamesPath := filepath.Join(dataDir, "surnames")

	if err := names.LoadNameFiles(namesPath, surnamesPath); err != nil {
		return fmt.Errorf("load name override from %q: %w", dataDir, err)
	}

	return nil
}

func waitForShutdown(errCh <-chan error) error {
	select {
	case err := <-errCh:
		if err == nil {
			logger.Info("Shutdown complete")
		}

		return err
	case <-time.After(shutdownGrace):
		logger.Warn("Shutdown timeout, forcing exit")

		return nil
	}
}
