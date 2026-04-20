// Package session wires runtime configuration to application mode entrypoints.
package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/openlibrecommunity/olcrtc/internal/carrier"
	"github.com/openlibrecommunity/olcrtc/internal/carrier/builtin"
	"github.com/openlibrecommunity/olcrtc/internal/client"
	"github.com/openlibrecommunity/olcrtc/internal/link"
	"github.com/openlibrecommunity/olcrtc/internal/link/direct"
	"github.com/openlibrecommunity/olcrtc/internal/server"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/openlibrecommunity/olcrtc/internal/transport/datachannel"
	"github.com/openlibrecommunity/olcrtc/internal/transport/seichannel"
	"github.com/openlibrecommunity/olcrtc/internal/transport/videochannel"
)

var (
	// ErrRoomIDRequired indicates that a room id is required for the selected carrier.
	ErrRoomIDRequired = errors.New("room ID required")
	// ErrModeRequired indicates that mode is not one of the supported values.
	ErrModeRequired = errors.New("specify -mode srv or -mode cnc")
	// ErrCarrierRequired indicates that no carrier was selected.
	ErrCarrierRequired = errors.New("carrier required (use -carrier telemost or -carrier jazz)")
	// ErrUnsupportedCarrier indicates that carrier is not registered.
	ErrUnsupportedCarrier = errors.New("unsupported carrier")
	// ErrUnsupportedLink indicates that link is not registered.
	ErrUnsupportedLink = errors.New("unsupported link")
	// ErrUnsupportedTransport indicates that transport is not registered.
	ErrUnsupportedTransport = errors.New("unsupported transport")
)

// Config holds runtime session settings.
type Config struct {
	Mode           string
	Link           string
	Transport      string
	Carrier        string
	RoomID         string
	KeyHex         string
	SOCKSHost      string
	SOCKSPort      int
	DNSServer      string
	SOCKSProxyAddr string
	SOCKSProxyPort int
	VideoWidth     int
	VideoHeight    int
	VideoFPS       int
	VideoBitrate   string
	VideoHW        string
}

// RegisterDefaults registers built-in providers and transports.
func RegisterDefaults() {
	builtin.Register()
	link.Register("direct", direct.New)
	transport.Register("datachannel", datachannel.New)
	transport.Register("videochannel", videochannel.New)
	transport.Register("seichannel", seichannel.New)
}

// Validate verifies that the runtime config refers to registered components.
func Validate(cfg Config) error {
	availableCarriers := carrier.Available()
	validCarrier := false
	for _, c := range availableCarriers {
		if cfg.Carrier == c {
			validCarrier = true
			break
		}
	}

	availableTransports := transport.Available()
	validTransport := false
	for _, t := range availableTransports {
		if cfg.Transport == t {
			validTransport = true
			break
		}
	}

	availableLinks := link.Available()
	validLink := false
	for _, l := range availableLinks {
		if cfg.Link == l {
			validLink = true
			break
		}
	}

	switch {
	case cfg.Carrier == "":
		return ErrCarrierRequired
	case !validCarrier:
		return fmt.Errorf("%w: %s (available: %v)", ErrUnsupportedCarrier, cfg.Carrier, availableCarriers)
	case !validLink:
		return fmt.Errorf("%w: %s (available: %v)", ErrUnsupportedLink, cfg.Link, availableLinks)
	case !validTransport:
		return fmt.Errorf("%w: %s (available: %v)", ErrUnsupportedTransport, cfg.Transport, availableTransports)
	case cfg.RoomID == "" && cfg.Carrier != "jazz":
		return ErrRoomIDRequired
	case cfg.Mode != "srv" && cfg.Mode != "cnc":
		return ErrModeRequired
	default:
		return nil
	}
}

// Run starts the configured mode.
func Run(ctx context.Context, cfg Config) error {
	roomURL := buildRoomURL(cfg.Carrier, cfg.RoomID)

	switch cfg.Mode {
	case "srv":
		return server.Run(
			ctx,
			cfg.Link,
			cfg.Transport,
			cfg.Carrier,
			roomURL,
			cfg.KeyHex,
			cfg.DNSServer,
			cfg.SOCKSProxyAddr,
			cfg.SOCKSProxyPort,
			cfg.VideoWidth,
			cfg.VideoHeight,
			cfg.VideoFPS,
			cfg.VideoBitrate,
			cfg.VideoHW,
		)
	case "cnc":
		return client.Run(
			ctx,
			cfg.Link,
			cfg.Transport,
			cfg.Carrier,
			roomURL,
			cfg.KeyHex,
			fmt.Sprintf("%s:%d", cfg.SOCKSHost, cfg.SOCKSPort),
			cfg.DNSServer,
			"",
			"",
			cfg.VideoWidth,
			cfg.VideoHeight,
			cfg.VideoFPS,
			cfg.VideoBitrate,
			cfg.VideoHW,
		)
	default:
		return ErrModeRequired
	}
}

func buildRoomURL(carrierName, roomID string) string {
	switch carrierName {
	case "telemost":
		return "https://telemost.yandex.ru/j/" + roomID
	case "jazz":
		if roomID == "" {
			return "any"
		}
		return roomID
	case "wb_stream":
		return roomID
	default:
		return roomID
	}
}
