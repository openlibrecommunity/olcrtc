package main

import (
	"flag"
	"log"

	"github.com/openlibrecommunity/olcrtc/internal/client"
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
	)

	flag.StringVar(&mode, "mode", "", "Mode: srv or cnc")
	flag.StringVar(&roomID, "id", "", "Telemost room ID")
	flag.StringVar(&provider, "provider", "telemost", "Provider (telemost only)")
	flag.IntVar(&socksPort, "socks-port", 1080, "SOCKS5 port (client only)")
	flag.StringVar(&keyHex, "key", "", "Shared encryption key (hex)")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging")
	flag.Parse()

	if !debug {
		log.SetFlags(log.Ltime)
	}

	if provider != "telemost" {
		log.Fatal("Only telemost provider supported")
	}

	if roomID == "" {
		log.Fatal("Room ID required")
	}

	roomURL := "https://telemost.yandex.ru/j/" + roomID

	switch mode {
	case "srv":
		if err := server.Run(roomURL, keyHex); err != nil {
			log.Fatal(err)
		}
	case "cnc":
		if err := client.Run(roomURL, keyHex, socksPort); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatal("Specify -mode srv or -mode cnc")
	}
}
