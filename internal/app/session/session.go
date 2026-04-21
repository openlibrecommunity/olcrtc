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
	ErrRoomIDRequired = errors.New("room ID required (use -id <id>)")
	// ErrModeRequired indicates that mode is not one of the supported values.
	ErrModeRequired = errors.New("mode required (use -mode srv or -mode cnc)")
	// ErrCarrierRequired indicates that no carrier was selected.
	ErrCarrierRequired = errors.New("carrier required (use -carrier telemost, -carrier jazz or -carrier wbstream)")
	// ErrUnsupportedCarrier indicates that carrier is not registered.
	ErrUnsupportedCarrier = errors.New("unsupported carrier")
	// ErrUnsupportedLink indicates that link is not registered.
	ErrUnsupportedLink = errors.New("unsupported link")
	// ErrUnsupportedTransport indicates that transport is not registered.
	ErrUnsupportedTransport = errors.New("unsupported transport")

	// ErrLinkRequired indicates that link is not provided.
	ErrLinkRequired = errors.New("link required (use -link direct)")
	// ErrTransportRequired indicates that transport is not provided.
	ErrTransportRequired = errors.New("transport required (use -transport datachannel, -transport videochannel or -transport seichannel)")
	// ErrKeyRequired indicates that encryption key is not provided.
	ErrKeyRequired = errors.New("key required (use -key <hex>)")
	// ErrDNSServerRequired indicates that dns server is not provided.
	ErrDNSServerRequired = errors.New("dns server required (use -dns 1.1.1.1:53)")

	// Videochannel errors
	ErrVideoWidthRequired   = errors.New("video width required for videochannel (use -video-w)")
	ErrVideoHeightRequired  = errors.New("video height required for videochannel (use -video-h)")
	ErrVideoFPSRequired     = errors.New("video fps required for videochannel (use -video-fps)")
	ErrVideoBitrateRequired = errors.New("video bitrate required for videochannel (use -video-bitrate)")
	ErrVideoHWRequired      = errors.New("video hardware acceleration required for videochannel (use -video-hw none/nvenc)")

	// CNC errors
	ErrSOCKSHostRequired = errors.New("socks host required for cnc mode (use -socks-host)")
	ErrSOCKSPortRequired = errors.New("socks port required for cnc mode (use -socks-port)")
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
	VideoQRSize    int
}

// RegisterDefaults registers built-in providers and transports.
func RegisterDefaults() {
	builtin.Register()
	link.Register("direct", direct.New)
	transport.Register("datachannel", datachannel.New)
	transport.Register("videochannel", videochannel.New)
	transport.Register("seichannel", seichannel.New)
}

// Validate verifies that the runtime config refers to registered components and all required fields are present.
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

	if cfg.Mode == "" {
		return ErrModeRequired
	}
	if cfg.Mode != "srv" && cfg.Mode != "cnc" {
		return ErrModeRequired
	}

	if cfg.Carrier == "" {
		return ErrCarrierRequired
	}
	if !validCarrier {
		return fmt.Errorf("%w: %s (available: %v)", ErrUnsupportedCarrier, cfg.Carrier, availableCarriers)
	}

	if cfg.Link == "" {
		return ErrLinkRequired
	}
	if !validLink {
		return fmt.Errorf("%w: %s (available: %v)", ErrUnsupportedLink, cfg.Link, availableLinks)
	}

	if cfg.Transport == "" {
		return ErrTransportRequired
	}
	if !validTransport {
		return fmt.Errorf("%w: %s (available: %v)", ErrUnsupportedTransport, cfg.Transport, availableTransports)
	}

	if cfg.RoomID == "" && cfg.Carrier != "jazz" {
		return ErrRoomIDRequired
	}

	if cfg.KeyHex == "" {
		return ErrKeyRequired
	}

	if cfg.DNSServer == "" {
		return ErrDNSServerRequired
	}

	if cfg.Transport == "videochannel" {
		if cfg.VideoWidth == 0 {
			return ErrVideoWidthRequired
		}
		if cfg.VideoHeight == 0 {
			return ErrVideoHeightRequired
		}
		if cfg.VideoFPS == 0 {
			return ErrVideoFPSRequired
		}
		if cfg.VideoBitrate == "" {
			return ErrVideoBitrateRequired
		}
		if cfg.VideoHW == "" {
			return ErrVideoHWRequired
		}
	}

	if cfg.Mode == "cnc" {
		if cfg.SOCKSHost == "" {
			return ErrSOCKSHostRequired
		}
		if cfg.SOCKSPort == 0 {
			return ErrSOCKSPortRequired
		}
	}

	return nil
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
			cfg.VideoQRSize,
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
			cfg.VideoQRSize,
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
	case "wbstream":
		return roomID
	default:
		return roomID
	}
}
