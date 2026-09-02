// Package config loads olcrtc runtime configuration from YAML files.
//
// The YAML schema mirrors [session.Config]. Parsing is strict: an unknown key
// is an error rather than a silently ignored typo. [Settings] is the part of
// the schema a failover profile may override, so the top-level file and every
// profile share exactly one definition and cannot drift apart.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/openlibrecommunity/olcrtc/internal/app/session"
)

var (
	// ErrConfigNotFound is returned when a config file path is set but the file does not exist.
	ErrConfigNotFound = errors.New("config file not found")
	// ErrConfigInvalidUTF8 is returned when a config file is not valid UTF-8.
	ErrConfigInvalidUTF8 = errors.New("config file is not valid UTF-8")
	// ErrCryptoKeyConflict is returned when both inline and file-backed keys are configured.
	ErrCryptoKeyConflict = errors.New("crypto.key and crypto.key_file cannot both be set")
	// ErrCryptoKeyFileEmpty is returned when crypto.key_file points to an empty file.
	ErrCryptoKeyFileEmpty = errors.New("crypto key file is empty")
)

// Settings is the overridable part of the schema. The top-level file and every
// failover profile share it, so a profile can override anything except the
// process-wide fields (mode, data, debug, gen, profiles, failover).
type Settings struct {
	Auth      Auth      `yaml:"auth"`
	Room      Room      `yaml:"room"`
	Crypto    Crypto    `yaml:"crypto"`
	Net       Net       `yaml:"net"`
	SOCKS     SOCKS     `yaml:"socks"`
	Engine    Engine    `yaml:"engine"`
	Video     Video     `yaml:"video"`
	VP8       VP8       `yaml:"vp8"`
	SEI       SEI       `yaml:"sei"`
	Liveness  Liveness  `yaml:"liveness"`
	Lifecycle Lifecycle `yaml:"lifecycle"`
	Traffic   Traffic   `yaml:"traffic"`
}

// File is the on-disk YAML schema.
type File struct {
	Mode string `yaml:"mode"`
	// Link is a deprecated no-op retained for one config schema migration cycle.
	//
	// Deprecated: remove this field from persisted configs.
	Link string `yaml:"link"`
	// FFmpeg is a deprecated no-op retained for one config schema migration cycle.
	//
	// Deprecated: remove this field from persisted configs.
	FFmpeg   string `yaml:"ffmpeg"`
	Settings `yaml:",inline"`
	Gen      Gen       `yaml:"gen"`
	Profiles []Profile `yaml:"profiles"`
	Failover Failover  `yaml:"failover"`
	Data     string    `yaml:"data"`
	Debug    bool      `yaml:"debug"`
}

// Profile is a failover entry that overrides top-level runtime fields.
type Profile struct {
	Name     string `yaml:"name"`
	Settings `yaml:",inline"`
}

// Failover controls ordered profile failover.
type Failover struct {
	RetryDelay string `yaml:"retry_delay"`
	MaxCycles  int    `yaml:"max_cycles"`
}

// Auth selects the auth provider.
type Auth struct {
	Provider string `yaml:"provider"` // jitsi, telemost, wbstream, none
	Token    string `yaml:"token"`    // optional pre-issued account token (wbstream)
}

// Room identifies the conference room.
type Room struct {
	ID      string `yaml:"id"`
	Channel string `yaml:"channel"`
}

// Crypto holds the shared secret(s) used to authenticate and encrypt the tunnel.
type Crypto struct {
	Key      string   `yaml:"key"`       // 64-char hex (32 bytes) - deprecated, use keys
	KeyFile  string   `yaml:"key_file"`  // path to a file containing crypto.key - deprecated, use keys_file
	Keys     []string `yaml:"keys"`      // list of 64-char hex keys (32 bytes each)
	KeysFile string   `yaml:"keys_file"` // path to a file containing multiple keys (one per line)
}

// Net groups network and transport selection.
type Net struct {
	Transport string `yaml:"transport"` // datachannel, videochannel, seichannel, vp8channel
	DNS       string `yaml:"dns"`
}

// SOCKS bundles SOCKS5 listener and outbound-proxy settings.
type SOCKS struct {
	Host      string `yaml:"host"`
	Port      int    `yaml:"port"`
	User      string `yaml:"user"`
	Pass      string `yaml:"pass"`
	ProxyAddr string `yaml:"proxy_addr"`
	ProxyPort int    `yaml:"proxy_port"`
	ProxyUser string `yaml:"proxy_user"`
	ProxyPass string `yaml:"proxy_pass"`
}

// Engine selects a direct SFU connection when Auth.Provider is "none".
type Engine struct {
	Name  string `yaml:"name"` // livekit, goolom, jitsi
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

// Video tunes the videochannel transport.
type Video struct {
	Width  int `yaml:"width"`
	Height int `yaml:"height"`
	FPS    int `yaml:"fps"`
	// Bitrate is a deprecated no-op retained for one config schema migration cycle.
	//
	// Deprecated: remove this field from persisted configs.
	Bitrate string `yaml:"bitrate"`
	// HW is a deprecated no-op retained for one config schema migration cycle.
	//
	// Deprecated: remove this field from persisted configs.
	HW         string `yaml:"hw"`
	QRSize     int    `yaml:"qr_size"`
	QRRecovery string `yaml:"qr_recovery"`
	Codec      string `yaml:"codec"`
	TileModule int    `yaml:"tile_module"`
	TileRS     int    `yaml:"tile_rs"`
}

// VP8 tunes the vp8channel transport.
type VP8 struct {
	FPS       int `yaml:"fps"`
	BatchSize int `yaml:"batch_size"`
}

// SEI tunes the seichannel transport.
type SEI struct {
	FPS          int `yaml:"fps"`
	BatchSize    int `yaml:"batch_size"`
	FragmentSize int `yaml:"fragment_size"`
	AckTimeoutMS int `yaml:"ack_timeout_ms"`
}

// Liveness tunes the post-handshake control stream ping/pong checks.
type Liveness struct {
	Interval string `yaml:"interval"`
	Timeout  string `yaml:"timeout"`
	Failures int    `yaml:"failures"`
}

// Lifecycle controls planned session rebuilds.
type Lifecycle struct {
	MaxSessionDuration string `yaml:"max_session_duration"`
}

// Traffic controls optional reliability-oriented send shaping.
type Traffic struct {
	MaxPayloadSize int    `yaml:"max_payload_size"`
	MinDelay       string `yaml:"min_delay"`
	MaxDelay       string `yaml:"max_delay"`
}

// Gen controls room-generation mode.
type Gen struct {
	Amount int `yaml:"amount"`
}

// Load parses a YAML file from disk. Unknown keys are rejected so a mistyped
// setting fails loudly instead of being silently ignored.
func Load(path string) (File, error) {
	// #nosec G304 -- config path is an explicit CLI/user input.
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return File{}, fmt.Errorf("%w: %s", ErrConfigNotFound, path)
		}

		return File{}, fmt.Errorf("read config %s: %w", path, err)
	}

	if !utf8.Valid(data) {
		return File{}, fmt.Errorf("parse config %s: %w", path, ErrConfigInvalidUTF8)
	}

	var file File

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	if err := decoder.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return File{}, fmt.Errorf("parse config %s: %w", path, err)
	}

	if err := loadExternalSecrets(path, &file); err != nil {
		return File{}, err
	}

	return file, nil
}

func loadExternalSecrets(configPath string, file *File) error {
	keys, err := resolveKeys(configPath, file.Crypto)
	if err != nil {
		return err
	}

	file.Crypto.Keys = keys

	for i := range file.Profiles {
		keys, err := resolveKeys(configPath, file.Profiles[i].Crypto)
		if err != nil {
			return fmt.Errorf("profiles[%d]: %w", i, err)
		}

		file.Profiles[i].Crypto.Keys = keys
	}

	return nil
}

func resolveKeys(configPath string, crypto Crypto) ([]string, error) {
	// Handle new keys/keys_file
	if len(crypto.Keys) > 0 {
		if crypto.KeysFile != "" {
			return nil, errors.New("crypto.keys and crypto.keys_file cannot both be set")
		}
		return crypto.Keys, nil
	}

	if crypto.KeysFile != "" {
		return readKeysFile(configPath, crypto.KeysFile)
	}

	// Handle legacy key/key_file for backward compatibility
	if crypto.KeyFile != "" && crypto.Key != "" {
		return nil, ErrCryptoKeyConflict
	}

	if crypto.KeyFile != "" {
		key, err := readKeyFile(configPath, crypto.KeyFile)
		if err != nil {
			return nil, err
		}
		return []string{key}, nil
	}

	if crypto.Key != "" {
		return []string{crypto.Key}, nil
	}

	return []string{}, nil
}

func readKeysFile(configPath, keysFile string) ([]string, error) {
	keysPath := keysFile
	if !filepath.IsAbs(keysPath) {
		keysPath = filepath.Join(filepath.Dir(configPath), keysPath)
	}

	// #nosec G304 -- keys_file is an explicit path in the user's config file.
	data, err := os.ReadFile(keysPath)
	if err != nil {
		return nil, fmt.Errorf("read crypto keys file %s: %w", keysPath, err)
	}

	if len(data) == 0 {
		return nil, ErrCryptoKeyFileEmpty
	}

	var keys []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, line)
	}

	if len(keys) == 0 {
		return nil, ErrCryptoKeyFileEmpty
	}

	return keys, nil
}

func readKeyFile(configPath, keyFile string) (string, error) {
	keyPath := keyFile
	if !filepath.IsAbs(keyPath) {
		keyPath = filepath.Join(filepath.Dir(configPath), keyPath)
	}

	// #nosec G304 -- key_file is an explicit path in the user's config file.
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("read crypto key file %s: %w", keyPath, err)
	}

	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", ErrCryptoKeyFileEmpty
	}

	return key, nil
}

// Apply converts a parsed file into a session config.
func Apply(file File) session.Config {
	cfg := ApplySettings(session.Config{}, file.Settings)
	cfg.Mode = file.Mode
	cfg.Amount = file.Gen.Amount

	return cfg
}

// ApplyProfile overlays a failover profile onto an already-applied base config.
func ApplyProfile(base session.Config, profile Profile) session.Config {
	return ApplySettings(base, profile.Settings)
}

// ApplySettings overlays every non-zero field of s onto dst. It is the single
// place that knows how the YAML schema maps onto [session.Config].
func ApplySettings(dst session.Config, s Settings) session.Config {
	dst.Transport = overlay(dst.Transport, s.Net.Transport)
	dst.DNSServer = overlay(dst.DNSServer, s.Net.DNS)

	dst.Provider = overlay(dst.Provider, s.Auth.Provider)
	dst.ProviderToken = overlay(dst.ProviderToken, s.Auth.Token)

	dst.Engine = overlay(dst.Engine, s.Engine.Name)
	dst.URL = overlay(dst.URL, s.Engine.URL)
	dst.Token = overlay(dst.Token, s.Engine.Token)

	dst.RoomID = overlay(dst.RoomID, s.Room.ID)
	dst.ChannelID = overlay(dst.ChannelID, s.Room.Channel)

	// Handle new Keys list, fall back to single Key for backward compatibility
	if len(s.Crypto.Keys) > 0 {
		dst.KeysHex = s.Crypto.Keys
	} else if s.Crypto.Key != "" {
		dst.KeysHex = []string{s.Crypto.Key}
	}
	// Also populate legacy KeyHex field with the first key for backward compatibility
	if len(dst.KeysHex) > 0 {
		dst.KeyHex = dst.KeysHex[0]
	}

	dst.SOCKSHost = overlay(dst.SOCKSHost, s.SOCKS.Host)
	dst.SOCKSPort = overlay(dst.SOCKSPort, s.SOCKS.Port)
	dst.SOCKSUser = overlay(dst.SOCKSUser, s.SOCKS.User)
	dst.SOCKSPass = overlay(dst.SOCKSPass, s.SOCKS.Pass)
	dst.SOCKSProxyAddr = overlay(dst.SOCKSProxyAddr, s.SOCKS.ProxyAddr)
	dst.SOCKSProxyPort = overlay(dst.SOCKSProxyPort, s.SOCKS.ProxyPort)
	dst.SOCKSProxyUser = overlay(dst.SOCKSProxyUser, s.SOCKS.ProxyUser)
	dst.SOCKSProxyPass = overlay(dst.SOCKSProxyPass, s.SOCKS.ProxyPass)

	dst.Video.Width = overlay(dst.Video.Width, s.Video.Width)
	dst.Video.Height = overlay(dst.Video.Height, s.Video.Height)
	dst.Video.FPS = overlay(dst.Video.FPS, s.Video.FPS)
	dst.Video.QRSize = overlay(dst.Video.QRSize, s.Video.QRSize)
	dst.Video.QRRecovery = overlay(dst.Video.QRRecovery, s.Video.QRRecovery)
	dst.Video.Codec = overlay(dst.Video.Codec, s.Video.Codec)
	dst.Video.TileModule = overlay(dst.Video.TileModule, s.Video.TileModule)
	dst.Video.TileRS = overlay(dst.Video.TileRS, s.Video.TileRS)

	dst.VP8.FPS = overlay(dst.VP8.FPS, s.VP8.FPS)
	dst.VP8.BatchSize = overlay(dst.VP8.BatchSize, s.VP8.BatchSize)

	dst.SEI.FPS = overlay(dst.SEI.FPS, s.SEI.FPS)
	dst.SEI.BatchSize = overlay(dst.SEI.BatchSize, s.SEI.BatchSize)
	dst.SEI.FragmentSize = overlay(dst.SEI.FragmentSize, s.SEI.FragmentSize)
	dst.SEI.AckTimeoutMS = overlay(dst.SEI.AckTimeoutMS, s.SEI.AckTimeoutMS)

	dst.LivenessInterval = overlay(dst.LivenessInterval, s.Liveness.Interval)
	dst.LivenessTimeout = overlay(dst.LivenessTimeout, s.Liveness.Timeout)
	dst.LivenessFailures = overlay(dst.LivenessFailures, s.Liveness.Failures)

	dst.MaxSessionDuration = overlay(dst.MaxSessionDuration, s.Lifecycle.MaxSessionDuration)

	dst.TrafficMaxPayloadSize = overlay(dst.TrafficMaxPayloadSize, s.Traffic.MaxPayloadSize)
	dst.TrafficMinDelay = overlay(dst.TrafficMinDelay, s.Traffic.MinDelay)
	dst.TrafficMaxDelay = overlay(dst.TrafficMaxDelay, s.Traffic.MaxDelay)

	return dst
}

// overlay returns override when it carries a value, otherwise base.
func overlay[T comparable](base, override T) T {
	var zero T
	if override != zero {
		return override
	}

	return base
}
