package session

import (
	"encoding/hex"
	"fmt"
	"net"
	"slices"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/openlibrecommunity/olcrtc/internal/control"
	enginebuiltin "github.com/openlibrecommunity/olcrtc/internal/engine/builtin"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
)

// Bounds for the numeric config fields that end up as buffer sizes, codec
// settings or listener parameters.
const (
	minVideoDimension  = 16
	maxVideoDimension  = 8192
	maxVideoTileModule = 270
	maxVideoTileRS     = 200
	maxSEIFragmentSize = 60000
	maxPort            = 65535
)

// videoQRRecoveryLevels are the error-correction levels the visual codec
// understands. Anything else silently degraded to the weakest level.
//
//nolint:gochecknoglobals // fixed lookup table
var videoQRRecoveryLevels = []string{defaultVideoQRRecovery, "medium", "high", "highest"}

// Validate verifies registered components and all required fields.
func Validate(cfg Config) error {
	checks := []func(Config) error{
		validateMode,
		validateProvider,
		validateTransportRegistration,
		validateCommon,
		validateTransportConfig,
		validateLivenessConfig,
		validateLifecycleConfig,
		validateTrafficConfig,
		validateModeConfig,
	}
	for _, check := range checks {
		if err := check(cfg); err != nil {
			return err
		}
	}
	return nil
}

func validateMode(cfg Config) error {
	switch cfg.Mode {
	case ModeSrv, ModeCnc, ModeGen:
		return nil
	default:
		return ErrModeRequired
	}
}

func validateProvider(cfg Config) error {
	if cfg.Provider == "" {
		return ErrProviderRequired
	}
	if !slices.Contains(enginebuiltin.Available(), cfg.Provider) {
		return fmt.Errorf("%w: %s (available: %v)", ErrUnsupportedProvider, cfg.Provider, enginebuiltin.Available())
	}
	return nil
}

func validateTransportRegistration(cfg Config) error {
	if cfg.Transport == "" {
		return ErrTransportRequired
	}
	if !slices.Contains(transport.Available(), cfg.Transport) {
		return fmt.Errorf("%w: %s (available: %v)", ErrUnsupportedTransport, cfg.Transport, transport.Available())
	}
	return nil
}

func validateCommon(cfg Config) error {
	if cfg.RoomID == "" && cfg.Provider != providerNone {
		return ErrRoomIDRequired
	}
	// Check new KeysHex format first, fall back to legacy KeyHex
	if len(cfg.KeysHex) > 0 {
		for i, keyHex := range cfg.KeysHex {
			if err := validateKey(keyHex); err != nil {
				return fmt.Errorf("keys[%d]: %w", i, err)
			}
		}
	} else if err := validateKey(cfg.KeyHex); err != nil {
		return err
	}
	if cfg.DNSServer == "" && cfg.Resolver == nil {
		return ErrDNSServerRequired
	}
	return nil
}

// validateKey rejects a malformed PSK here rather than deep inside Run. The
// failover supervisor restarts a profile forever, so a mistyped key would
// otherwise turn into a silent restart loop instead of one startup error.
func validateKey(keyHex string) error {
	if keyHex == "" {
		return ErrKeyRequired
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != chacha20poly1305.KeySize {
		return ErrKeyInvalid
	}
	return nil
}

// validateFPS bounds every transport's frame rate. The writer loops derive
// their ticker period from time.Second/fps, which truncates to zero for absurd
// values and panics the writer goroutine.
func validateFPS(fps int, missing error) error {
	if fps == 0 {
		return missing
	}
	if fps < 0 || fps > transport.MaxFPS {
		return fmt.Errorf("%w: %d", ErrFPSInvalid, fps)
	}
	return nil
}

func validateTransportConfig(cfg Config) error {
	switch cfg.Transport {
	case transportVideo:
		return validateVideoChannel(cfg)
	case transportVP8:
		return validateVP8Channel(cfg)
	case transportSEI:
		return validateSEIChannel(cfg)
	default:
		return nil
	}
}

func validateVideoCodec(cfg Config) error {
	if cfg.Video.Codec != "" && cfg.Video.Codec != videoCodecQRCode && cfg.Video.Codec != videoCodecTile {
		return ErrVideoCodecInvalid
	}
	if cfg.Video.Codec == videoCodecTile && (cfg.Video.Width != 1080 || cfg.Video.Height != 1080) {
		return ErrTileCodecDimensions
	}
	return nil
}

// validateVideoVisual bounds the parameters the visual codec turns into frame
// buffers and codec settings. Out-of-range values used to pass here and only
// surface once the tunnel had joined the room, at which point every frame
// failed; an unbounded width/height pair reserved the product in bytes.
func validateVideoVisual(cfg Config) error {
	if cfg.Video.Width < minVideoDimension || cfg.Video.Width > maxVideoDimension ||
		cfg.Video.Height < minVideoDimension || cfg.Video.Height > maxVideoDimension {
		return fmt.Errorf("%w: %dx%d", ErrVideoDimensionsInvalid, cfg.Video.Width, cfg.Video.Height)
	}
	if cfg.Video.QRRecovery != "" && !slices.Contains(videoQRRecoveryLevels, cfg.Video.QRRecovery) {
		return fmt.Errorf("%w: %s", ErrVideoQRRecoveryInvalid, cfg.Video.QRRecovery)
	}
	if cfg.Video.TileModule < 0 || cfg.Video.TileModule > maxVideoTileModule {
		return fmt.Errorf("%w: %d", ErrVideoTileModuleInvalid, cfg.Video.TileModule)
	}
	if cfg.Video.TileRS < 0 || cfg.Video.TileRS > maxVideoTileRS {
		return fmt.Errorf("%w: %d", ErrVideoTileRSInvalid, cfg.Video.TileRS)
	}
	return nil
}

func validateVideoChannel(cfg Config) error {
	if cfg.Video.Width == 0 {
		return ErrVideoWidthRequired
	}
	if cfg.Video.Height == 0 {
		return ErrVideoHeightRequired
	}
	if err := validateFPS(cfg.Video.FPS, ErrVideoFPSRequired); err != nil {
		return err
	}
	if err := validateVideoCodec(cfg); err != nil {
		return err
	}
	return validateVideoVisual(cfg)
}

func validateVP8Channel(cfg Config) error {
	if err := validateFPS(cfg.VP8.FPS, ErrVP8FPSRequired); err != nil {
		return err
	}
	if cfg.VP8.BatchSize == 0 {
		return ErrVP8BatchSizeRequired
	}
	return nil
}

func validateSEIChannel(cfg Config) error {
	if err := validateFPS(cfg.SEI.FPS, ErrSEIFPSRequired); err != nil {
		return err
	}
	if cfg.SEI.BatchSize == 0 {
		return ErrSEIBatchSizeRequired
	}
	if cfg.SEI.FragmentSize == 0 {
		return ErrSEIFragmentSizeRequired
	}
	if cfg.SEI.FragmentSize < 0 || cfg.SEI.FragmentSize > maxSEIFragmentSize {
		return fmt.Errorf("%w: %d", ErrSEIFragmentSizeInvalid, cfg.SEI.FragmentSize)
	}
	if cfg.SEI.AckTimeoutMS == 0 {
		return ErrSEIAckTimeoutRequired
	}
	return nil
}

func validateModeConfig(cfg Config) error {
	if cfg.Mode != ModeCnc {
		return nil
	}
	if cfg.SOCKSHost == "" {
		return ErrSOCKSHostRequired
	}
	if cfg.SOCKSPort == 0 {
		return ErrSOCKSPortRequired
	}
	if cfg.SOCKSPort < 0 || cfg.SOCKSPort > maxPort {
		return fmt.Errorf("%w: %d", ErrSOCKSPortInvalid, cfg.SOCKSPort)
	}
	if !isLoopbackListenHost(cfg.SOCKSHost) && (cfg.SOCKSUser == "" || cfg.SOCKSPass == "") {
		return ErrSOCKSAuthRequired
	}
	return nil
}

func validateLivenessConfig(cfg Config) error {
	if _, err := parseLivenessDuration(cfg.LivenessInterval, control.DefaultInterval); err != nil {
		return fmt.Errorf("%w: %w", ErrLivenessIntervalInvalid, err)
	}
	if _, err := parseLivenessDuration(cfg.LivenessTimeout, control.DefaultTimeout); err != nil {
		return fmt.Errorf("%w: %w", ErrLivenessTimeoutInvalid, err)
	}
	if cfg.LivenessFailures < 0 {
		return ErrLivenessFailuresInvalid
	}
	return nil
}

func validateLifecycleConfig(cfg Config) error {
	_, err := maxSessionDuration(cfg)
	return err
}

func parseLivenessDuration(value string, def time.Duration) (time.Duration, error) {
	if value == "" {
		return def, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse duration: %w", err)
	}
	if duration <= 0 {
		return 0, errPositiveDuration
	}
	return duration, nil
}

func livenessConfig(cfg Config) (control.Config, error) {
	interval, err := parseLivenessDuration(cfg.LivenessInterval, control.DefaultInterval)
	if err != nil {
		return control.Config{}, fmt.Errorf("%w: %w", ErrLivenessIntervalInvalid, err)
	}
	timeout, err := parseLivenessDuration(cfg.LivenessTimeout, control.DefaultTimeout)
	if err != nil {
		return control.Config{}, fmt.Errorf("%w: %w", ErrLivenessTimeoutInvalid, err)
	}
	failures := cfg.LivenessFailures
	if failures == 0 {
		failures = control.DefaultFailures
	}
	if failures < 0 {
		return control.Config{}, ErrLivenessFailuresInvalid
	}
	return control.Config{Interval: interval, Timeout: timeout, Failures: failures}, nil
}

func maxSessionDuration(cfg Config) (time.Duration, error) {
	if cfg.MaxSessionDuration == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(cfg.MaxSessionDuration)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrLifecycleMaxSessionDurationInvalid, err)
	}
	if duration <= 0 {
		return 0, ErrLifecycleMaxSessionDurationInvalid
	}
	return duration, nil
}

func validateTrafficConfig(cfg Config) error {
	_, err := trafficConfig(cfg)
	return err
}

func trafficConfig(cfg Config) (transport.TrafficConfig, error) {
	if cfg.TrafficMaxPayloadSize < 0 || (cfg.TrafficMaxPayloadSize > 0 &&
		cfg.TrafficMaxPayloadSize < runtime.MinSmuxWirePayload) {
		return transport.TrafficConfig{}, ErrTrafficMaxPayloadSizeInvalid
	}
	minDelay, err := parseOptionalNonNegativeDuration(cfg.TrafficMinDelay)
	if err != nil {
		return transport.TrafficConfig{}, fmt.Errorf("%w: %w", ErrTrafficMinDelayInvalid, err)
	}
	maxDelay, err := parseOptionalNonNegativeDuration(cfg.TrafficMaxDelay)
	if err != nil {
		return transport.TrafficConfig{}, fmt.Errorf("%w: %w", ErrTrafficMaxDelayInvalid, err)
	}
	if maxDelay > 0 && maxDelay < minDelay {
		return transport.TrafficConfig{}, ErrTrafficMaxDelayInvalid
	}
	return transport.TrafficConfig{
		MaxPayloadSize: cfg.TrafficMaxPayloadSize,
		MinDelay:       minDelay,
		MaxDelay:       maxDelay,
	}, nil
}

func parseOptionalNonNegativeDuration(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse duration: %w", err)
	}
	if duration < 0 {
		return 0, errNonNegativeDuration
	}
	return duration, nil
}

func isLoopbackListenHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
