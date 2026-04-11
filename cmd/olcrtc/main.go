// main
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/client"
	"github.com/openlibrecommunity/olcrtc/internal/errors"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/names"
	"github.com/openlibrecommunity/olcrtc/internal/server"
)

func main() {
	cfg := parseFlags()

	if err := run(cfg); err != nil {
		log.Fatal(err)
	}
}

type config struct {
	mode      string
	roomID    string
	provider  string
	socksPort int
	keyHex    string
	debug     bool
	dataDir   string
	duo       bool
	dnsServer string
}

func parseFlags() *config {
	cfg := &config{}

	flag.StringVar(&cfg.mode, "mode", "", "Mode: srv or cnc")
	flag.StringVar(&cfg.roomID, "id", "", "Telemost room ID")
	flag.StringVar(&cfg.provider, "provider", "telemost", "Provider (telemost only)")
	flag.IntVar(&cfg.socksPort, "socks-port", 1080, "SOCKS5 port (client only)")
	flag.StringVar(&cfg.keyHex, "key", "", "Shared encryption key (hex)")
	flag.BoolVar(&cfg.debug, "debug", false, "Enable verbose logging")
	flag.StringVar(&cfg.dataDir, "data", "data", "Path to data directory")
	flag.BoolVar(&cfg.duo, "duo", false, "Use dual channels for 2x throughput")
	flag.StringVar(&cfg.dnsServer, "dns", "1.1.1.1:53", "DNS server (default: Cloudflare 1.1.1.1)")
	flag.Parse()

	return cfg
}

func run(cfg *config) error {
	setupLogging(cfg.debug)

	if cfg.provider != "telemost" {
		return errors.ErrOnlyTelemost
	}

	if cfg.roomID == "" {
		return errors.ErrRoomIDRequired
	}

	if cfg.mode != "srv" && cfg.mode != "cnc" {
		return errors.ErrModeRequired
	}

	dataDir := resolveDataDir(cfg.dataDir)

	namesPath := filepath.Join(dataDir, "names")
	surnamesPath := filepath.Join(dataDir, "surnames")

	if err := names.LoadNameFiles(namesPath, surnamesPath); err != nil {
		return fmt.Errorf("failed to load name files: %w", err)
	}

	roomURL := "https://telemost.yandex.ru/j/" + cfg.roomID

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)

	go func() {
		switch cfg.mode {
		case "srv":
			errCh <- server.Run(ctx, roomURL, cfg.keyHex, cfg.duo, cfg.dnsServer)
		case "cnc":
			errCh <- client.Run(ctx, roomURL, cfg.keyHex, cfg.socksPort, cfg.duo)
		}
	}()

	return waitForShutdown(ctx, cancel, sigCh, errCh)
}

func setupLogging(debug bool) {
	if debug {
		log.SetFlags(log.Ltime | log.Lshortfile)
		logger.SetVerbose(true)
	} else {
		log.SetFlags(log.Ltime)
	}
}

func resolveDataDir(dataDir string) string {
	if !filepath.IsAbs(dataDir) {
		exePath, err := os.Executable()
		if err == nil {
			exeDir := filepath.Dir(exePath)
			dataDir = filepath.Join(exeDir, dataDir)
		}
	}
	return dataDir
}

func waitForShutdown(_ context.Context, cancel context.CancelFunc, sigCh <-chan os.Signal, errCh <-chan error) error {
	select {
	case <-sigCh:
		log.Println("Shutting down gracefully...")
		cancel()

		done := make(chan struct{})
		go func() {
			<-errCh
			close(done)
		}()

		select {
		case <-done:
			log.Println("Shutdown complete")
		case <-time.After(5 * time.Second):
			log.Println("Shutdown timeout, forcing exit")
		}
		return nil
	case err := <-errCh:
		return err
	}
}