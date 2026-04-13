// Package main provides the olcrtc CLI entrypoint.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/client"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/names"
	"github.com/openlibrecommunity/olcrtc/internal/server"
)

type config struct {
	mode           string
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
}

var (
	errUnsupportedProvider = errors.New("only telemost provider supported")
	errRoomIDRequired      = errors.New("room ID required")
	errModeRequired        = errors.New("specify -mode srv or -mode cnc")
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	cfg := parseFlags()
	configureLogging(cfg.debug)

	if err := validateConfig(cfg); err != nil {
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
	go runMode(ctx, cfg, errCh)

	select {
	case <-sigCh:
		log.Println("Shutting down gracefully...")
		cancel()
		return waitForShutdown(errCh)
	case err := <-errCh:
		return err
	}
}

func parseFlags() config {
	cfg := config{}

	flag.StringVar(&cfg.mode, "mode", "", "Mode: srv or cnc")
	flag.StringVar(&cfg.roomID, "id", "", "Telemost room ID")
	flag.StringVar(&cfg.provider, "provider", "telemost", "Provider (telemost only)")
	flag.IntVar(&cfg.socksPort, "socks-port", 1080, "SOCKS5 port (client only)")
	flag.StringVar(&cfg.socksHost, "socks-host", "127.0.0.1", "SOCKS5 listen host (client only)")
	flag.StringVar(&cfg.keyHex, "key", "", "Shared encryption key (hex)")
	flag.BoolVar(&cfg.debug, "debug", false, "Enable verbose logging")
	flag.StringVar(&cfg.dataDir, "data", "data", "Path to data directory")
	flag.StringVar(&cfg.dnsServer, "dns", "77.88.8.8:53", "DNS server (default: Yandex 77.88.8.8)")
	flag.StringVar(&cfg.socksProxyAddr, "socks-proxy", "", "SOCKS5 proxy address (server only)")
	flag.IntVar(&cfg.socksProxyPort, "socks-proxy-port", 1080, "SOCKS5 proxy port (server only)")
	flag.Parse()

	return cfg
}

func configureLogging(debug bool) {
	if debug {
		log.SetFlags(log.Ltime | log.Lshortfile)
		logger.SetVerbose(true)
		return
	}

	log.SetFlags(log.Ltime)
}

func validateConfig(cfg config) error {
	switch {
	case cfg.provider != "telemost":
		return errUnsupportedProvider
	case cfg.roomID == "":
		return errRoomIDRequired
	case cfg.mode != "srv" && cfg.mode != "cnc":
		return errModeRequired
	default:
		return nil
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

func runMode(ctx context.Context, cfg config, errCh chan<- error) {
	roomURL := "https://telemost.yandex.ru/j/" + cfg.roomID

	switch cfg.mode {
	case "srv":
		errCh <- server.Run(
			ctx,
			roomURL,
			cfg.keyHex,
			cfg.dnsServer,
			cfg.socksProxyAddr,
			cfg.socksProxyPort,
		)
	case "cnc":
		errCh <- client.Run(
			ctx,
			roomURL,
			cfg.keyHex,
			cfg.socksPort,
			cfg.socksHost,
			"",
			"",
			cfg.dnsServer,
		)
	}
}

func waitForShutdown(errCh <-chan error) error {
	done := make(chan error, 1)
	go func() {
		done <- <-errCh
	}()

	select {
	case err := <-done:
		if err == nil {
			log.Println("Shutdown complete")
		}
		return err
	case <-time.After(5 * time.Second):
		log.Println("Shutdown timeout, forcing exit")
		return nil
	}
}
