// Package session wires runtime configuration to application mode entrypoints.
package session

import (
	"errors"
	"net"

	enginebuiltin "github.com/openlibrecommunity/olcrtc/internal/engine/builtin"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/openlibrecommunity/olcrtc/internal/transport/datachannel"
	"github.com/openlibrecommunity/olcrtc/internal/transport/seichannel"
	"github.com/openlibrecommunity/olcrtc/internal/transport/videochannel"
	"github.com/openlibrecommunity/olcrtc/internal/transport/vp8channel"
)

// Supported values for the mode config field.
const (
	ModeSrv = "srv"
	ModeCnc = "cnc"
	ModeGen = "gen"
)

const (
	providerNone     = "none"
	transportVideo   = "videochannel"
	transportVP8     = "vp8channel"
	transportSEI     = "seichannel"
	videoCodecQRCode = "qrcode"
	videoCodecTile   = "tile"
)

const (
	defaultVideoWidth      = 1920
	defaultVideoHeight     = 1080
	defaultVideoFPS        = 30
	defaultVideoQRRecovery = "low"
	defaultVP8FPS          = 30
	defaultVP8BatchSize    = 64
	defaultSEIFPS          = 30
	defaultSEIBatchSize    = 64
	defaultSEIFragmentSize = 900
	defaultSEIAckTimeoutMS = 2000
)

var (
	ErrRoomIDRequired   = errors.New("room ID required (set room.id)")
	ErrModeRequired     = errors.New("mode required (set mode to srv, cnc or gen)")
	ErrAmountRequired   = errors.New("amount required for gen mode (set gen.amount)")
	ErrProviderRequired = errors.New(
		"auth provider required (set auth.provider to jitsi, telemost, wbstream or none)")
	ErrUnsupportedProvider  = errors.New("unsupported provider")
	ErrUnsupportedTransport = errors.New("unsupported transport")
	ErrTransportRequired    = errors.New(
		"transport required (set transport to datachannel, videochannel, seichannel or vp8channel)")
	ErrKeyRequired         = errors.New("key required (set crypto.key)")
	ErrDNSServerRequired   = errors.New("dns server required (set net.dns)")
	ErrVideoWidthRequired  = errors.New("video width required for videochannel (set video.width)")
	ErrVideoHeightRequired = errors.New("video height required for videochannel (set video.height)")
	ErrVideoFPSRequired    = errors.New("video fps required for videochannel (set video.fps)")
	ErrVideoCodecInvalid   = errors.New(
		"invalid video codec for videochannel (set video.codec to qrcode or tile)")
	ErrTileCodecDimensions    = errors.New("tile codec requires video.width: 1080 and video.height: 1080")
	ErrVideoDimensionsInvalid = errors.New(
		"invalid video dimensions (set video.width and video.height within 16..8192)")
	ErrVideoQRRecoveryInvalid = errors.New(
		"invalid video qr recovery (set video.qr_recovery to low, medium, high or highest)")
	ErrVideoTileModuleInvalid  = errors.New("invalid video tile module (set video.tile_module within 1..270)")
	ErrVideoTileRSInvalid      = errors.New("invalid video tile rs (set video.tile_rs within 0..200)")
	ErrFPSInvalid              = errors.New("invalid fps (set it within 1..240)")
	ErrKeyInvalid              = errors.New("invalid key (set crypto.key to 64 hex characters)")
	ErrSOCKSPortInvalid        = errors.New("invalid socks port (set socks.port within 1..65535)")
	ErrSEIFragmentSizeInvalid  = errors.New("invalid sei fragment size (set sei.fragment_size within 1..60000)")
	ErrVP8FPSRequired          = errors.New("vp8 fps required for vp8channel (set vp8.fps)")
	ErrVP8BatchSizeRequired    = errors.New("vp8 batch size required for vp8channel (set vp8.batch_size)")
	ErrSEIFPSRequired          = errors.New("fps required for seichannel (set sei.fps)")
	ErrSEIBatchSizeRequired    = errors.New("batch size required for seichannel (set sei.batch_size)")
	ErrSEIFragmentSizeRequired = errors.New("fragment size required for seichannel (set sei.fragment_size)")
	ErrSEIAckTimeoutRequired   = errors.New("ack timeout required for seichannel (set sei.ack_timeout_ms)")
	ErrSOCKSHostRequired       = errors.New("socks host required for cnc mode (set socks.host)")
	ErrSOCKSPortRequired       = errors.New("socks port required for cnc mode (set socks.port)")
	ErrSOCKSAuthRequired       = errors.New(
		"socks auth required when binding outside loopback (set socks.user and socks.pass)")
	ErrLivenessIntervalInvalid = errors.New(
		"invalid liveness interval (set liveness.interval to a duration > 0)")
	ErrLivenessTimeoutInvalid = errors.New(
		"invalid liveness timeout (set liveness.timeout to a duration > 0)")
	ErrLivenessFailuresInvalid = errors.New(
		"invalid liveness failures (set liveness.failures to a value > 0)")
	ErrLifecycleMaxSessionDurationInvalid = errors.New(
		"invalid max session duration (set lifecycle.max_session_duration to a duration > 0)")
	ErrTrafficMaxPayloadSizeInvalid = errors.New(
		"invalid traffic max payload size (set traffic.max_payload_size to 0 or a value above crypto overhead)")
	ErrTrafficMinDelayInvalid = errors.New(
		"invalid traffic min delay (set traffic.min_delay to a duration >= 0)")
	ErrTrafficMaxDelayInvalid = errors.New(
		"invalid traffic max delay (set traffic.max_delay to a duration >= 0 and >= traffic.min_delay)")
	errPositiveDuration    = errors.New("duration must be > 0")
	errNonNegativeDuration = errors.New("duration must be >= 0")
)

// VideoConfig holds tunables for the videochannel transport.
type VideoConfig struct {
	Width      int
	Height     int
	FPS        int
	QRSize     int
	QRRecovery string
	Codec      string
	TileModule int
	TileRS     int
}

// VP8Config holds tunables for the vp8channel transport.
type VP8Config struct {
	FPS       int
	BatchSize int
}

// SEIConfig holds tunables for the seichannel transport.
type SEIConfig struct {
	FPS          int
	BatchSize    int
	FragmentSize int
	AckTimeoutMS int
}

// Config holds runtime session settings.
type Config struct {
	Mode                  string
	Transport             string
	Provider              string
	ProviderToken         string
	Engine                string
	URL                   string
	Token                 string
	RoomID                string
	ChannelID             string
	KeyHex                string   // deprecated, use KeysHex
	KeysHex               []string // list of encryption keys
	SOCKSHost             string
	SOCKSPort             int
	SOCKSUser             string
	SOCKSPass             string
	DNSServer             string
	Resolver              *net.Resolver
	SOCKSProxyAddr        string
	SOCKSProxyPort        int
	SOCKSProxyUser        string
	SOCKSProxyPass        string
	Video                 VideoConfig
	VP8                   VP8Config
	SEI                   SEIConfig
	LivenessInterval      string
	LivenessTimeout       string
	LivenessFailures      int
	MaxSessionDuration    string
	TrafficMaxPayloadSize int
	TrafficMinDelay       string
	TrafficMaxDelay       string
	Amount                int
}

// RegisterDefaults registers built-in providers and transports.
func RegisterDefaults() {
	enginebuiltin.RegisterDefaults()
	transport.Register("datachannel", datachannel.New)
	transport.Register("videochannel", videochannel.New)
	transport.Register("seichannel", seichannel.New)
	transport.Register("vp8channel", vp8channel.New)
}
