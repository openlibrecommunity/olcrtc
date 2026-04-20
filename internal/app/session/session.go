// Package session wires runtime configuration to application mode entrypoints.
package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/openlibrecommunity/olcrtc/internal/carrier"
	"github.com/openlibrecommunity/olcrtc/internal/client"
	"github.com/openlibrecommunity/olcrtc/internal/provider/jazz"
	"github.com/openlibrecommunity/olcrtc/internal/provider/telemost"
	"github.com/openlibrecommunity/olcrtc/internal/provider/wbstream"
	"github.com/openlibrecommunity/olcrtc/internal/server"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/openlibrecommunity/olcrtc/internal/transport/datachannel"
)

var (
	// ErrRoomIDRequired indicates that a room id is required for the selected provider.
	ErrRoomIDRequired = errors.New("room ID required")
	// ErrModeRequired indicates that mode is not one of the supported values.
	ErrModeRequired = errors.New("specify -mode srv or -mode cnc")
	// ErrCarrierRequired indicates that no carrier was selected.
	ErrCarrierRequired = errors.New("carrier required (use -carrier telemost or -carrier jazz)")
	// ErrUnsupportedCarrier indicates that carrier is not registered.
	ErrUnsupportedCarrier = errors.New("unsupported carrier")
	// ErrUnsupportedTransport indicates that transport is not registered.
	ErrUnsupportedTransport = errors.New("unsupported transport")
)

// Config holds runtime session settings.
type Config struct {
	Mode           string
	Transport      string
	Carrier        string
	RoomID         string
	KeyHex         string
	SOCKSHost      string
	SOCKSPort      int
	DNSServer      string
	SOCKSProxyAddr string
	SOCKSProxyPort int
}

// RegisterDefaults registers built-in providers and transports.
func RegisterDefaults() {
	carrier.Register("jazz", jazz.New)
	carrier.Register("telemost", telemost.New)
	carrier.Register("wb_stream", wbstream.New)

	transport.Register("datachannel", datachannel.New)
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

	switch {
	case cfg.Carrier == "":
		return ErrCarrierRequired
	case !validCarrier:
		return fmt.Errorf("%w: %s (available: %v)", ErrUnsupportedCarrier, cfg.Carrier, availableCarriers)
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
			cfg.Transport,
			cfg.Carrier,
			roomURL,
			cfg.KeyHex,
			cfg.DNSServer,
			cfg.SOCKSProxyAddr,
			cfg.SOCKSProxyPort,
		)
	case "cnc":
		return client.Run(
			ctx,
			cfg.Transport,
			cfg.Carrier,
			roomURL,
			cfg.KeyHex,
			fmt.Sprintf("%s:%d", cfg.SOCKSHost, cfg.SOCKSPort),
			cfg.DNSServer,
			"",
			"",
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
