// Package main provides the olcrtc CLI entrypoint.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/app/session"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/names"
)

type config struct {
	mode           string
	link           string
	transport      string
	carrier        string
	roomID         string
	provider       string
	socksPort      int
	socksHost      string
	keyHex         string
	debug          bool
	dataDir        string
	dnsServer      string
	socksProxyAddr string
	socksProxyPort int
	videoWidth     int
	videoHeight    int
	videoFPS       int
	videoBitrate   string
	videoHW        string
}

func main() {
	if err := run(); err != nil {
		logger.Error(err)
		os.Exit(1)
	}
}

func run() error {
	session.RegisterDefaults()

	cfg := parseFlags()
	configureLogging(cfg.debug)

	if err := session.Validate(toSessionConfig(cfg)); err != nil {
		return err
	}

	dataDir, err := resolveDataDir(cfg.dataDir)
	if err != nil {
		return err
	}

	if err := loadNames(dataDir); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		errCh <- session.Run(ctx, toSessionConfig(cfg))
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

func parseFlags() config {
	cfg := config{}

	flag.StringVar(&cfg.mode, "mode", "", "Mode: srv or cnc")
	flag.StringVar(&cfg.link, "link", "direct", "Link: direct")
	flag.StringVar(&cfg.transport, "transport", "datachannel", "Transport: datachannel, videochannel, seichannel")
	flag.StringVar(&cfg.carrier, "carrier", "", "Carrier: telemost, jazz, wb_stream")
	flag.StringVar(&cfg.roomID, "id", "", "Room ID")
	flag.StringVar(&cfg.provider, "provider", "", "Deprecated alias for -carrier")
	flag.IntVar(&cfg.socksPort, "socks-port", 1080, "SOCKS5 port (client only)")
	flag.StringVar(&cfg.socksHost, "socks-host", "127.0.0.1", "SOCKS5 listen host (client only)")
	flag.StringVar(&cfg.keyHex, "key", "", "Shared encryption key (hex)")
	flag.BoolVar(&cfg.debug, "debug", false, "Enable verbose logging")
	flag.StringVar(&cfg.dataDir, "data", "data", "Path to data directory")
	flag.StringVar(&cfg.dnsServer, "dns", "1.1.1.1:53", "DNS server (default: Cloudflare 1.1.1.1)")
	flag.StringVar(&cfg.socksProxyAddr, "socks-proxy", "", "SOCKS5 proxy address (server only)")
	flag.IntVar(&cfg.socksProxyPort, "socks-proxy-port", 1080, "SOCKS5 proxy port (server only)")
	flag.IntVar(&cfg.videoWidth, "video-w", 640, "Video logical width (videochannel only)")
	flag.IntVar(&cfg.videoHeight, "video-h", 360, "Video logical height (videochannel only)")
	flag.IntVar(&cfg.videoFPS, "video-fps", 25, "Video frames per second (videochannel only)")
	flag.StringVar(&cfg.videoBitrate, "video-bitrate", "2048k", "Video bitrate (videochannel only)")
	flag.Parse()

	return cfg
}

func configureLogging(debug bool) {
	if debug {
		logger.SetVerbose(true)
	}
}

func resolveDataDir(dataDir string) (string, error) {
	if filepath.IsAbs(dataDir) {
		return dataDir, nil
	}

	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}

	return filepath.Join(filepath.Dir(exePath), dataDir), nil
}

func loadNames(dataDir string) error {
	namesPath := filepath.Join(dataDir, "names")
	surnamesPath := filepath.Join(dataDir, "surnames")
	if err := names.LoadNameFiles(namesPath, surnamesPath); err != nil {
		return fmt.Errorf("load embedded names override: %w", err)
	}

	return nil
}

func toSessionConfig(cfg config) session.Config {
	return session.Config{
		Mode:           cfg.mode,
		Link:           cfg.link,
		Transport:      cfg.transport,
		Carrier:        firstNonEmpty(cfg.carrier, cfg.provider),
		RoomID:         cfg.roomID,
		KeyHex:         cfg.keyHex,
		SOCKSHost:      cfg.socksHost,
		SOCKSPort:      cfg.socksPort,
		DNSServer:      cfg.dnsServer,
		SOCKSProxyAddr: cfg.socksProxyAddr,
		SOCKSProxyPort: cfg.socksProxyPort,
		VideoWidth:     cfg.videoWidth,
		VideoHeight:    cfg.videoHeight,
		VideoFPS:       cfg.videoFPS,
		VideoBitrate:   cfg.videoBitrate,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func waitForShutdown(errCh <-chan error) error {
	done := make(chan error, 1)
	go func() {
		done <- <-errCh
	}()

	select {
	case err := <-done:
		if err == nil {
			logger.Info("Shutdown complete")
		}
		return err
	case <-time.After(5 * time.Second):
		logger.Warn("Shutdown timeout, forcing exit")
		return nil
	}
}
