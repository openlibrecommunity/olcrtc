package session

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/client"
	"github.com/openlibrecommunity/olcrtc/internal/control"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/server"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/openlibrecommunity/olcrtc/internal/tunnelcore"
)

const defaultSessionRestartDelay = 2 * time.Second

// Run applies defaults, validates the config, and starts the selected mode.
func Run(ctx context.Context, cfg Config) error {
	RegisterDefaults()
	prepared, err := prepareRunConfig(cfg)
	if err != nil {
		return err
	}
	cfg = prepared
	cfg.Resolver = tunnelcore.Resolver(cfg.Resolver, cfg.DNSServer)
	liveness, err := livenessConfig(cfg)
	if err != nil {
		return err
	}
	maxDuration, err := maxSessionDuration(cfg)
	if err != nil {
		return err
	}
	traffic, err := trafficConfig(cfg)
	if err != nil {
		return err
	}
	run := func(ctx context.Context) error {
		return runOnce(ctx, cfg, cfg.RoomID, liveness, traffic)
	}
	if maxDuration > 0 {
		return runWithSessionRotation(ctx, maxDuration, defaultSessionRestartDelay, run)
	}
	return run(ctx)
}

func prepareRunConfig(cfg Config) (Config, error) {
	prepared := ApplyDefaults(cfg)
	if err := Validate(prepared); err != nil {
		return Config{}, err
	}
	return prepared, nil
}

func runOnce(
	ctx context.Context,
	cfg Config,
	roomURL string,
	liveness control.Config,
	traffic transport.TrafficConfig,
) error {
	opts := buildTransportOptions(cfg)
	switch cfg.Mode {
	case ModeSrv:
		return runServer(ctx, cfg, roomURL, liveness, traffic, opts)
	case ModeCnc:
		return runClient(ctx, cfg, roomURL, liveness, traffic, opts)
	default:
		return ErrModeRequired
	}
}

func runServer(
	ctx context.Context,
	cfg Config,
	roomURL string,
	liveness control.Config,
	traffic transport.TrafficConfig,
	opts transport.Options,
) error {
	err := server.Run(ctx, server.Config{
		Transport: cfg.Transport, Provider: cfg.Provider, RoomURL: roomURL, ChannelID: cfg.ChannelID,
		KeyHex: cfg.KeyHex, KeysHex: cfg.KeysHex, DNSServer: cfg.DNSServer, Resolver: cfg.Resolver,
		SOCKSProxyAddr: cfg.SOCKSProxyAddr, SOCKSProxyPort: cfg.SOCKSProxyPort,
		SOCKSProxyUser: cfg.SOCKSProxyUser, SOCKSProxyPass: cfg.SOCKSProxyPass,
		TransportOptions: opts, Engine: cfg.Engine, URL: cfg.URL, Token: cfg.Token,
		ProviderToken: cfg.ProviderToken, Liveness: liveness, Traffic: traffic,
		OnSessionOpen: func(sessionID, deviceID string, claims map[string]any) {
			logger.Infof("session opened: id=%s device=%s claims=%v", sessionID, deviceID, claims)
		},
		OnSessionClose: func(sessionID, reason string) {
			logger.Infof("session closed: id=%s reason=%s", sessionID, reason)
		},
		OnTraffic: func(sessionID, addr string, bytesIn, bytesOut uint64) {
			logger.Infof("traffic: session=%s addr=%s in=%d out=%d", sessionID, addr, bytesIn, bytesOut)
		},
	})
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}

func runClient(
	ctx context.Context,
	cfg Config,
	roomURL string,
	liveness control.Config,
	traffic transport.TrafficConfig,
	opts transport.Options,
) error {
	err := client.Run(ctx, client.Config{
		Transport: cfg.Transport, Provider: cfg.Provider, RoomURL: roomURL, ChannelID: cfg.ChannelID,
		KeyHex: cfg.KeyHex, KeysHex: cfg.KeysHex, LocalAddr: fmt.Sprintf("%s:%d", cfg.SOCKSHost, cfg.SOCKSPort),
		DNSServer: cfg.DNSServer, Resolver: cfg.Resolver, SOCKSUser: cfg.SOCKSUser,
		SOCKSPass: cfg.SOCKSPass, TransportOptions: opts, Engine: cfg.Engine,
		URL: cfg.URL, Token: cfg.Token, ProviderToken: cfg.ProviderToken,
		Liveness: liveness, Traffic: traffic,
	})
	if err != nil {
		return fmt.Errorf("client: %w", err)
	}
	return nil
}

func runWithSessionRotation(
	ctx context.Context,
	maxDuration time.Duration,
	restartDelay time.Duration,
	run func(context.Context) error,
) error {
	for cycle := 1; ; cycle++ {
		rotated, err := runSessionCycle(ctx, cycle, maxDuration, run)
		if ctx.Err() != nil {
			return nil //nolint:nilerr // parent cancellation is normal shutdown
		}
		if err != nil && !rotated {
			return err
		}
		if err != nil {
			logger.Warnf("session rotation ended with error: cycle=%d err=%v", cycle, err)
		}
		logger.Infof("session rotation restarting: next_cycle=%d", cycle+1)
		if err := waitSessionRestart(ctx, restartDelay); err != nil {
			return nil //nolint:nilerr // canceled restart delay is normal shutdown
		}
	}
}

func runSessionCycle(
	ctx context.Context,
	cycle int,
	maxDuration time.Duration,
	run func(context.Context) error,
) (bool, error) {
	runCtx, cancel := context.WithCancel(ctx)
	var rotated atomic.Bool
	timer := time.AfterFunc(maxDuration, func() {
		rotated.Store(true)
		logger.Infof("session max duration reached: duration=%s cycle=%d", maxDuration, cycle)
		cancel()
	})
	err := run(runCtx)
	cancel()
	timer.Stop()
	if !rotated.Load() && err == nil {
		logger.Infof("session ended cleanly with lifecycle rotation enabled: next_cycle=%d", cycle+1)
	}
	return rotated.Load(), err
}

func waitSessionRestart(ctx context.Context, delay time.Duration) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("restart delay canceled: %w", ctx.Err())
	case <-time.After(delay):
		return nil
	}
}
