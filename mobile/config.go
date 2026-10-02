package mobile

import (
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
	"github.com/openlibrecommunity/olcrtc/pkg/olcrtc/client"
)

const (
	transportData  = "datachannel"
	transportVP8   = "vp8channel"
	transportSEI   = "seichannel"
	transportVideo = "videochannel"
)

const (
	defaultTransport        = transportVP8
	defaultDNSServer        = "8.8.8.8:53"
	defaultSOCKSHost        = "127.0.0.1"
	defaultSOCKSPort        = 1080
	defaultVP8FPS           = 30
	defaultVP8BatchSize     = 64
	defaultSEIFPS           = 30
	defaultSEIBatchSize     = 64
	defaultSEIFragmentSize  = 900
	defaultSEIAckTimeoutMS  = 2000
	defaultVideoWidth       = 1920
	defaultVideoHeight      = 1080
	defaultVideoFPS         = 30
	videoCodecQRCode        = "qrcode"
	videoCodecTile          = "tile"
	defaultVideoCodec       = videoCodecQRCode
	defaultVideoRecovery    = "low"
	defaultVideoTileModule  = 4
	defaultLivenessInterval = 10 * time.Second
	defaultLivenessTimeout  = 15 * time.Second
	defaultLivenessFailures = 4
	minTrafficPayloadSize   = 53
)

type runtimeConfig struct {
	provider  string
	transport string
	roomURL   string
	channelID string
	keyHex    string
	dnsServer string
	// resolver is an override handed in by SetResolver; dns is the runtime's
	// own, which SetDNS steers and every run without an override uses.
	resolver      protect.Lookup
	dns           *protect.Resolver
	socksHost     string
	socksPort     int
	socksUser     string
	socksPass     string
	providerToken string
	deviceID      string
	deviceIDPath  string
	engine        string
	serviceURL    string
	engineToken   string
	liveness      client.LivenessConfig
	traffic       client.TrafficConfig
	vp8           client.VP8Options
	sei           client.SEIOptions
	video         client.VideoOptions
}

func defaultRuntimeConfig() runtimeConfig {
	return runtimeConfig{
		transport: defaultTransport,
		dnsServer: defaultDNSServer,
		dns:       protect.NewResolver(defaultDNSServer),
		socksHost: defaultSOCKSHost,
		socksPort: defaultSOCKSPort,
		liveness: client.LivenessConfig{
			Interval: defaultLivenessInterval,
			Timeout:  defaultLivenessTimeout,
			Failures: defaultLivenessFailures,
		},
		vp8: client.VP8Options{FPS: defaultVP8FPS, BatchSize: defaultVP8BatchSize},
		sei: client.SEIOptions{
			FPS: defaultSEIFPS, BatchSize: defaultSEIBatchSize,
			FragmentSize: defaultSEIFragmentSize, AckTimeoutMS: defaultSEIAckTimeoutMS,
		},
		video: client.VideoOptions{
			Width: defaultVideoWidth, Height: defaultVideoHeight, FPS: defaultVideoFPS,
			Codec: defaultVideoCodec, QRRecovery: defaultVideoRecovery,
			TileModule: defaultVideoTileModule,
		},
	}
}

// SetProvider selects jitsi, telemost, wbstream, or none.
func (r *Runtime) SetProvider(provider string) error {
	if !supportedProvider(provider) {
		return fmt.Errorf("%w: %q", ErrUnsupportedProvider, provider)
	}
	r.mu.Lock()
	r.defaults.provider = provider
	r.mu.Unlock()
	return nil
}

// SetTransport selects one of the four built-in transports.
func (r *Runtime) SetTransport(transport string) error {
	if !supportedTransport(transport) {
		return fmt.Errorf("%w: %q", ErrUnsupportedTransport, transport)
	}
	r.mu.Lock()
	r.defaults.transport = transport
	r.mu.Unlock()
	return nil
}

// SetRoom sets the provider room ID or URL.
func (r *Runtime) SetRoom(roomURL string) error {
	roomURL = strings.TrimSpace(roomURL)
	if roomURL == "" {
		return fmt.Errorf("%w: room is required", ErrInvalidConfig)
	}
	r.mu.Lock()
	r.defaults.roomURL = roomURL
	r.mu.Unlock()
	return nil
}

// SetChannel sets the optional peer-routing channel ID.
func (r *Runtime) SetChannel(channelID string) {
	r.mu.Lock()
	r.defaults.channelID = strings.TrimSpace(channelID)
	r.mu.Unlock()
}

// SetKey sets and validates the 32-byte hexadecimal tunnel key.
func (r *Runtime) SetKey(keyHex string) error {
	if err := validateKey(keyHex); err != nil {
		return err
	}
	r.mu.Lock()
	r.defaults.keyHex = keyHex
	r.mu.Unlock()
	return nil
}

// SetDNS sets the DNS servers this Runtime resolves names through: one
// "host:port", or several separated by commas, semicolons or spaces, asked in
// that order. The platform passes the resolvers of the network it stands on
// first and a public operator behind them; the engine adds that operator's
// IPv6 twin and the other public operators after it, and asks the host's own
// resolver when none of them answers.
//
// A running generation follows the change at once: the platform calls this
// when the network under the tunnel changes, and the next lookup should ask
// that network's resolvers rather than the previous one's.
func (r *Runtime) SetDNS(dnsServer string) error {
	if err := validateDNSServers(dnsServer); err != nil {
		return err
	}
	r.mu.Lock()
	r.defaults.dnsServer = dnsServer
	r.defaults.dns.SetServers(dnsServer)
	r.mu.Unlock()
	return nil
}

// SetResolver overrides the resolver used by future runs. Nil restores the
// runtime's own DNS resolver.
func (r *Runtime) SetResolver(resolver protect.Lookup) {
	r.mu.Lock()
	r.defaults.resolver = resolver
	r.mu.Unlock()
}

// SetSocksListenHost sets the local SOCKS5 bind host.
func (r *Runtime) SetSocksListenHost(host string) error {
	host = cleanHost(host)
	if host == "" {
		return fmt.Errorf("%w: SOCKS host is required", ErrInvalidConfig)
	}
	r.mu.Lock()
	r.defaults.socksHost = host
	r.mu.Unlock()
	return nil
}

// SetSocksPort sets the local SOCKS5 bind port.
func (r *Runtime) SetSocksPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%w: SOCKS port must be between 1 and 65535", ErrInvalidConfig)
	}
	r.mu.Lock()
	r.defaults.socksPort = port
	r.mu.Unlock()
	return nil
}

// SetSocksCredentials sets optional local SOCKS5 authentication.
func (r *Runtime) SetSocksCredentials(user, password string) error {
	if len(user) > 255 || len(password) > 255 {
		return fmt.Errorf("%w: SOCKS credentials exceed 255 bytes", ErrInvalidConfig)
	}
	r.mu.Lock()
	r.defaults.socksUser = user
	r.defaults.socksPass = password
	r.mu.Unlock()
	return nil
}

// SetProviderToken sets an optional pre-issued provider account token.
func (r *Runtime) SetProviderToken(token string) {
	r.mu.Lock()
	r.defaults.providerToken = strings.TrimSpace(token)
	r.mu.Unlock()
}

// SetDeviceID sets the in-memory device identity used by future runs.
func (r *Runtime) SetDeviceID(deviceID string) {
	r.mu.Lock()
	r.defaults.deviceID = strings.TrimSpace(deviceID)
	r.mu.Unlock()
}

// SetDeviceIDPath sets the persistent device identity file path.
func (r *Runtime) SetDeviceIDPath(path string) {
	r.mu.Lock()
	r.defaults.deviceIDPath = strings.TrimSpace(path)
	r.mu.Unlock()
}

// SetEngine configures direct engine access used with provider none.
func (r *Runtime) SetEngine(name, serviceURL, token string) {
	r.mu.Lock()
	r.defaults.engine = strings.TrimSpace(name)
	r.defaults.serviceURL = strings.TrimSpace(serviceURL)
	r.defaults.engineToken = strings.TrimSpace(token)
	r.mu.Unlock()
}

// SetLivenessOptions configures control-stream checks in milliseconds.
func (r *Runtime) SetLivenessOptions(intervalMillis, timeoutMillis, failures int) error {
	if intervalMillis <= 0 || timeoutMillis <= 0 || failures <= 0 {
		return fmt.Errorf("%w: liveness values must be positive", ErrInvalidConfig)
	}
	interval, intervalOK := checkedDurationMillis(intervalMillis)
	timeout, timeoutOK := checkedDurationMillis(timeoutMillis)
	if !intervalOK || !timeoutOK {
		return fmt.Errorf("%w: liveness duration is too large", ErrInvalidConfig)
	}
	r.mu.Lock()
	r.defaults.liveness = client.LivenessConfig{
		Interval: interval,
		Timeout:  timeout,
		Failures: failures,
	}
	r.mu.Unlock()
	return nil
}

// SetTrafficOptions configures payload limits and pacing in milliseconds.
func (r *Runtime) SetTrafficOptions(maxPayloadSize, minDelayMillis, maxDelayMillis int) error {
	if maxPayloadSize < 0 || (maxPayloadSize > 0 && maxPayloadSize < minTrafficPayloadSize) {
		return fmt.Errorf("%w: traffic payload limit is too small", ErrInvalidConfig)
	}
	if minDelayMillis < 0 || maxDelayMillis < 0 || (maxDelayMillis > 0 && maxDelayMillis < minDelayMillis) {
		return fmt.Errorf("%w: invalid traffic delay range", ErrInvalidConfig)
	}
	minDelay, minDelayOK := checkedDurationMillis(minDelayMillis)
	maxDelay, maxDelayOK := checkedDurationMillis(maxDelayMillis)
	if !minDelayOK || !maxDelayOK {
		return fmt.Errorf("%w: traffic delay is too large", ErrInvalidConfig)
	}
	r.mu.Lock()
	r.defaults.traffic = client.TrafficConfig{
		MaxPayloadSize: maxPayloadSize,
		MinDelay:       minDelay,
		MaxDelay:       maxDelay,
	}
	r.mu.Unlock()
	return nil
}

// SetVP8Options configures vp8channel.
func (r *Runtime) SetVP8Options(fps, batchSize int) error {
	if fps < 1 || fps > 120 || batchSize < 1 {
		return fmt.Errorf("%w: invalid VP8 options", ErrInvalidConfig)
	}
	r.mu.Lock()
	r.defaults.vp8 = client.VP8Options{FPS: fps, BatchSize: batchSize}
	r.mu.Unlock()
	return nil
}

// SetSEIOptions configures seichannel.
func (r *Runtime) SetSEIOptions(fps, batchSize, fragmentSize, ackTimeoutMillis int) error {
	if fps < 1 || fps > 120 || batchSize < 1 || fragmentSize < 1 || ackTimeoutMillis < 1 {
		return fmt.Errorf("%w: invalid SEI options", ErrInvalidConfig)
	}
	r.mu.Lock()
	r.defaults.sei = client.SEIOptions{
		FPS: fps, BatchSize: batchSize, FragmentSize: fragmentSize, AckTimeoutMS: ackTimeoutMillis,
	}
	r.mu.Unlock()
	return nil
}

// SetVideoOptions configures videochannel.
func (r *Runtime) SetVideoOptions(
	width, height, fps, qrSize int,
	qrRecovery, codec string,
	tileModule, tileRS int,
) error {
	if err := validateVideoOptions(width, height, fps, qrSize, qrRecovery, codec, tileModule, tileRS); err != nil {
		return err
	}
	r.mu.Lock()
	r.defaults.video = client.VideoOptions{
		Width: width, Height: height, FPS: fps, QRSize: qrSize,
		QRRecovery: qrRecovery, Codec: codec, TileModule: tileModule, TileRS: tileRS,
	}
	r.mu.Unlock()
	return nil
}

// SetDebug controls the process-wide internal logger verbosity without changing stdlib log output.
func (r *Runtime) SetDebug(enabled bool) {
	logger.SetVerbose(enabled)
}

func (cfg runtimeConfig) clientConfig() client.Config {
	return client.Config{
		Transport: cfg.transport, Provider: cfg.provider, RoomURL: cfg.roomURL,
		ChannelID: cfg.channelID, Engine: cfg.engine, URL: cfg.serviceURL, Token: cfg.engineToken,
		ProviderToken: cfg.providerToken, KeyHex: cfg.keyHex,
		LocalAddr: net.JoinHostPort(cfg.socksHost, strconv.Itoa(cfg.socksPort)),
		SOCKSUser: cfg.socksUser, SOCKSPass: cfg.socksPass,
		DNSServer: cfg.dnsServer, Resolver: cfg.lookup(),
		TransportOptions: cfg.transportOptions(), Liveness: cfg.liveness, Traffic: cfg.traffic,
		DeviceID: cfg.deviceID, DeviceIDPath: cfg.deviceIDPath,
	}
}

// lookup is what a run resolves names with: the override, else the runtime's own.
func (cfg runtimeConfig) lookup() protect.Lookup {
	if cfg.resolver != nil {
		return cfg.resolver
	}
	return cfg.dns
}

func (cfg runtimeConfig) transportOptions() client.TransportOptions {
	switch cfg.transport {
	case transportVP8:
		return cfg.vp8
	case transportSEI:
		return cfg.sei
	case transportVideo:
		return cfg.video
	default:
		return nil
	}
}

func validateRuntimeConfig(cfg runtimeConfig) error {
	if !supportedProvider(cfg.provider) {
		return fmt.Errorf("%w: provider %q", ErrInvalidConfig, cfg.provider)
	}
	if !supportedTransport(cfg.transport) {
		return fmt.Errorf("%w: transport %q", ErrInvalidConfig, cfg.transport)
	}
	if cfg.provider != "none" && cfg.roomURL == "" {
		return fmt.Errorf("%w: room is required", ErrInvalidConfig)
	}
	if err := validateKey(cfg.keyHex); err != nil {
		return err
	}
	if cfg.dnsServer == "" && cfg.resolver == nil {
		return fmt.Errorf("%w: DNS server or resolver is required", ErrInvalidConfig)
	}
	if cfg.socksHost == "" || cfg.socksPort < 1 || cfg.socksPort > 65535 {
		return fmt.Errorf("%w: invalid SOCKS listen address", ErrInvalidConfig)
	}
	if !isLoopbackHost(cfg.socksHost) && (cfg.socksUser == "" || cfg.socksPass == "") {
		return fmt.Errorf("%w: SOCKS credentials are required outside loopback", ErrInvalidConfig)
	}
	return nil
}

func validateVideoOptions(
	width, height, fps, qrSize int,
	qrRecovery, codec string,
	tileModule, tileRS int,
) error {
	if width < 1 || height < 1 || fps < 1 || fps > 120 || qrSize < 0 {
		return fmt.Errorf("%w: invalid video dimensions, FPS, or QR size", ErrInvalidConfig)
	}
	if codec != videoCodecQRCode && codec != videoCodecTile {
		return fmt.Errorf("%w: video codec must be qrcode or tile", ErrInvalidConfig)
	}
	if !supportedQRRecovery(qrRecovery) {
		return fmt.Errorf("%w: invalid QR recovery", ErrInvalidConfig)
	}
	return validateTileOptions(width, height, codec, tileModule, tileRS)
}

func validateTileOptions(width, height int, codec string, tileModule, tileRS int) error {
	if tileModule < 1 || tileModule > 270 || tileRS < 0 || tileRS > 200 {
		return fmt.Errorf("%w: invalid tile options", ErrInvalidConfig)
	}
	if codec == videoCodecTile && (width != 1080 || height != 1080) {
		return fmt.Errorf("%w: tile codec requires 1080x1080", ErrInvalidConfig)
	}
	return nil
}

func validateKey(keyHex string) error {
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return fmt.Errorf("%w: decode key: %w", ErrInvalidConfig, err)
	}
	if len(key) != 32 {
		return fmt.Errorf("%w: key must be 32 bytes", ErrInvalidConfig)
	}
	return nil
}

// validateDNSServers accepts the list shapes SetDNS documents and rejects an
// empty list, a bad port or an empty host.
func validateDNSServers(list string) error {
	entries := protect.SplitDNSServers(list)
	if len(entries) == 0 {
		return fmt.Errorf("%w: DNS server is required", ErrInvalidConfig)
	}
	for _, entry := range entries {
		if err := validateDNSEntry(entry); err != nil {
			return err
		}
	}
	return nil
}

func validateDNSEntry(entry string) error {
	host, portText, err := net.SplitHostPort(entry)
	if err != nil {
		host, portText = strings.Trim(entry, "[]"), "53"
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("%w: DNS server %q has no host", ErrInvalidConfig, entry)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%w: DNS server %q has a bad port", ErrInvalidConfig, entry)
	}
	return nil
}

func supportedProvider(provider string) bool {
	switch provider {
	case "jitsi", "telemost", "wbstream", "none":
		return true
	default:
		return false
	}
}

func supportedTransport(transport string) bool {
	switch transport {
	case transportData, transportVP8, transportSEI, transportVideo:
		return true
	default:
		return false
	}
}

func supportedQRRecovery(value string) bool {
	switch value {
	case "low", "medium", "high", "highest":
		return true
	default:
		return false
	}
}

func cleanHost(host string) string {
	host = strings.TrimSpace(host)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		return strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	return host
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func checkedDurationMillis(value int) (time.Duration, bool) {
	if value < 0 || int64(value) > maxTimeoutMillis {
		return 0, false
	}
	return time.Duration(value) * time.Millisecond, true
}
