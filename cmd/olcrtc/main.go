package main

import (
	"context"
	"flag"
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

func main() {
	var (
		mode      string
		roomID    string
		provider  string
		socksPort int
		keyHex    string
		debug     bool
		dataDir   string
		duo       bool
		dnsServer string
	)

	flag.StringVar(&mode, "mode", "", "Mode: srv or cnc")
	flag.StringVar(&roomID, "id", "", "Telemost room ID")
	flag.StringVar(&provider, "provider", "telemost", "Provider (telemost only)")
	flag.IntVar(&socksPort, "socks-port", 1080, "SOCKS5 port (client only)")
	flag.StringVar(&keyHex, "key", "", "Shared encryption key (hex)")
	flag.BoolVar(&debug, "debug", false, "Enable verbose logging")
	flag.StringVar(&dataDir, "data", "data", "Path to data directory")
	flag.BoolVar(&duo, "duo", false, "Use dual channels for 2x throughput")
	flag.StringVar(&dnsServer, "dns", "1.1.1.1:53", "DNS server (default: Cloudflare 1.1.1.1)")
	flag.Parse()

	if debug {
		log.SetFlags(log.Ltime | log.Lshortfile)
		logger.SetVerbose(true)
	} else {
		log.SetFlags(log.Ltime)
	}

	if provider != "telemost" {
		log.Fatal("Only telemost provider supported")
	}

	if roomID == "" {
		log.Fatal("Room ID required")
	}

	if mode != "srv" && mode != "cnc" {
		log.Fatal("Specify -mode srv or -mode cnc")
	}

	if !filepath.IsAbs(dataDir) {
		exePath, err := os.Executable()
		if err == nil {
			exeDir := filepath.Dir(exePath)
			dataDir = filepath.Join(exeDir, dataDir)
		}
	}

	namesPath := filepath.Join(dataDir, "names")
	surnamesPath := filepath.Join(dataDir, "surnames")

	if err := names.LoadNameFiles(namesPath, surnamesPath); err != nil {
		log.Fatalf("Failed to load name files: %v", err)
	}

	roomURL := "https://telemost.yandex.ru/j/" + roomID

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)

	go func() {
		switch mode {
		case "srv":
			errCh <- server.Run(ctx, roomURL, keyHex, duo, dnsServer)
		case "cnc":
			errCh <- client.Run(ctx, roomURL, keyHex, socksPort, duo)
		}
	}()

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
	case err := <-errCh:
		if err != nil {
			log.Fatal(err)
		}
	}
}
