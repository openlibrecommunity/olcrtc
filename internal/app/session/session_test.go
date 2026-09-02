package session

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/control"
	enginebuiltin "github.com/openlibrecommunity/olcrtc/internal/engine/builtin"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/openlibrecommunity/olcrtc/internal/tunnelcore"
)

const testBadDuration = "nope"

func TestRegisterDefaultsConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(RegisterDefaults)
	}
	wg.Wait()

	for _, name := range []string{"jitsi", "none", "telemost", "wbstream"} {
		if !slices.Contains(enginebuiltin.Available(), name) {
			t.Fatalf("provider %q is not registered", name)
		}
	}
	if !slices.IsSorted(enginebuiltin.Available()) || !slices.IsSorted(transport.Available()) {
		t.Fatal("registered names are not sorted")
	}
}

func TestApplyTransportDefaults(t *testing.T) {
	tests := []struct {
		name string
		in   Config
		want Config
	}{
		{
			name: "vp8",
			in:   Config{Transport: transportVP8},
			want: Config{Transport: transportVP8, VP8: VP8Config{FPS: 30, BatchSize: 64}},
		},
		{
			name: "sei",
			in:   Config{Transport: transportSEI},
			want: Config{
				Transport: transportSEI,
				SEI:       SEIConfig{FPS: 30, BatchSize: 64, FragmentSize: 900, AckTimeoutMS: 2000},
			},
		},
		{
			name: "video qrcode",
			in:   Config{Transport: transportVideo},
			want: Config{
				Transport: transportVideo,
				Video: VideoConfig{
					Width: 1920, Height: 1080, FPS: 30,
					QRRecovery: "low", Codec: videoCodecQRCode,
				},
			},
		},
		{
			name: "video tile dimensions",
			in:   Config{Transport: transportVideo, Video: VideoConfig{Codec: videoCodecTile}},
			want: Config{
				Transport: transportVideo,
				Video: VideoConfig{
					Width: 1080, Height: 1080, FPS: 30,
					QRRecovery: "low", Codec: videoCodecTile,
				},
			},
		},
		{
			name: "keeps explicit values",
			in: Config{
				Transport: transportSEI,
				SEI:       SEIConfig{FPS: 10, BatchSize: 2, FragmentSize: 300, AckTimeoutMS: 1500},
			},
			want: Config{
				Transport: transportSEI,
				SEI:       SEIConfig{FPS: 10, BatchSize: 2, FragmentSize: 300, AckTimeoutMS: 1500},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplyTransportDefaults(tt.in)
			if got.Transport != tt.want.Transport {
				t.Fatalf("ApplyTransportDefaults() Transport = %q, want %q", got.Transport, tt.want.Transport)
			}
			if got.VP8 != tt.want.VP8 && got.SEI != tt.want.SEI && got.Video != tt.want.Video {
				t.Fatalf("ApplyTransportDefaults() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestApplyLivenessDefaults(t *testing.T) {
	got := ApplyLivenessDefaults(Config{})
	if got.LivenessInterval != control.DefaultInterval.String() {
		t.Fatalf("LivenessInterval = %q, want %q", got.LivenessInterval, control.DefaultInterval.String())
	}
	if got.LivenessTimeout != control.DefaultTimeout.String() {
		t.Fatalf("LivenessTimeout = %q, want %q", got.LivenessTimeout, control.DefaultTimeout.String())
	}
	if got.LivenessFailures != control.DefaultFailures {
		t.Fatalf("LivenessFailures = %d, want %d", got.LivenessFailures, control.DefaultFailures)
	}

	explicit := Config{LivenessInterval: "1s", LivenessTimeout: "500ms", LivenessFailures: 9}
	if got := ApplyLivenessDefaults(explicit); got.LivenessInterval != "1s" || got.LivenessTimeout != "500ms" || got.LivenessFailures != 9 {
		t.Fatalf("ApplyLivenessDefaults() = %+v, want %+v", got, explicit)
	}
}

func TestResolverForDoesNotMutateDefaultResolver(t *testing.T) {
	defaultResolver := net.DefaultResolver
	custom := &net.Resolver{PreferGo: true}

	resolver := tunnelcore.Resolver(nil, "8.8.8.8:53")
	if net.DefaultResolver != defaultResolver {
		t.Fatal("resolverFor() mutated net.DefaultResolver")
	}
	if resolver == nil || resolver == net.DefaultResolver {
		t.Fatal("resolverFor() did not create a local resolver")
	}

	if tunnelcore.Resolver(custom, "8.8.8.8:53") != custom {
		t.Fatal("resolverFor() did not prefer the supplied resolver")
	}
}

func TestRunWithSessionRotationRestartsAfterMaxDuration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	err := runWithSessionRotation(ctx, 5*time.Millisecond, time.Millisecond, func(ctx context.Context) error {
		if calls.Add(1) >= 2 {
			cancel()
			return nil
		}
		<-ctx.Done()
		return nil
	})
	if err != nil {
		t.Fatalf("runWithSessionRotation() error = %v", err)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("run calls = %d, want at least 2", got)
	}
}

func TestPrepareRunConfigAppliesDefaultsThenValidates(t *testing.T) {
	RegisterDefaults()
	cfg, err := prepareRunConfig(Config{
		Mode:      ModeSrv,
		Transport: transportVP8,
		Provider:  "telemost",
		RoomID:    "room-1",
		KeyHex:    "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		DNSServer: "8.8.8.8:53",
	})
	if err != nil {
		t.Fatalf("prepareRunConfig() error = %v", err)
	}
	if cfg.VP8.FPS != defaultVP8FPS || cfg.VP8.BatchSize != defaultVP8BatchSize {
		t.Fatalf("VP8 defaults = %+v", cfg.VP8)
	}
	if cfg.LivenessInterval == "" || cfg.LivenessTimeout == "" || cfg.LivenessFailures == 0 {
		t.Fatalf("liveness defaults = %+v", cfg)
	}
	if _, err := prepareRunConfig(Config{}); !errors.Is(err, ErrModeRequired) {
		t.Fatalf("prepareRunConfig(empty) error = %v, want %v", err, ErrModeRequired)
	}
	if err := Run(context.Background(), Config{}); !errors.Is(err, ErrModeRequired) {
		t.Fatalf("Run(empty) error = %v, want %v", err, ErrModeRequired)
	}
}

func TestValidate(t *testing.T) {
	RegisterDefaults()

	base := Config{
		Mode:      ModeSrv,
		Transport: "datachannel",
		Provider:  "telemost",
		RoomID:    "room-1",
		KeyHex:    "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		DNSServer: "8.8.8.8:53",
	}

	tests := []struct {
		name string
		cfg  Config
		want error
	}{
		{name: "valid baseline", cfg: base},
		{
			name: "custom resolver without dns server",
			cfg: func() Config {
				cfg := base
				cfg.DNSServer = ""
				cfg.Resolver = &net.Resolver{PreferGo: true}
				return cfg
			}(),
		},
		{
			name: "cnc requires socks host and port",
			cfg: func() Config {
				cfg := base
				cfg.Mode = ModeCnc
				cfg.SOCKSHost = "127.0.0.1"
				cfg.SOCKSPort = 1080
				return cfg
			}(),
		},
		{
			name: "missing mode",
			cfg: func() Config {
				cfg := base
				cfg.Mode = ""
				return cfg
			}(),
			want: ErrModeRequired,
		},
		{
			name: "unsupported provider",
			cfg: func() Config {
				cfg := base
				cfg.Provider = "unknown"
				return cfg
			}(),
			want: ErrUnsupportedProvider,
		},
		{
			name: "unsupported transport",
			cfg: func() Config {
				cfg := base
				cfg.Transport = "unknown"
				return cfg
			}(),
			want: ErrUnsupportedTransport,
		},
		{
			name: "room id required",
			cfg: func() Config {
				cfg := base
				cfg.RoomID = ""
				return cfg
			}(),
			want: ErrRoomIDRequired,
		},
		{
			name: "key required",
			cfg: func() Config {
				cfg := base
				cfg.KeyHex = ""
				return cfg
			}(),
			want: ErrKeyRequired,
		},
		{
			name: "dns server required",
			cfg: func() Config {
				cfg := base
				cfg.DNSServer = ""
				return cfg
			}(),
			want: ErrDNSServerRequired,
		},
		{
			name: "videochannel requires dimensions and fps",
			cfg: func() Config {
				cfg := base
				cfg.Transport = "videochannel"
				return cfg
			}(),
			want: ErrVideoWidthRequired,
		},
		{
			name: "videochannel rejects invalid codec",
			cfg: func() Config {
				cfg := base
				cfg.Transport = "videochannel"
				cfg.Video.Width = 640
				cfg.Video.Height = 480
				cfg.Video.FPS = 30
				cfg.Video.Codec = "bogus"
				return cfg
			}(),
			want: ErrVideoCodecInvalid,
		},
		{
			name: "videochannel requires height",
			cfg: func() Config {
				cfg := base
				cfg.Transport = "videochannel"
				cfg.Video.Width = 640
				return cfg
			}(),
			want: ErrVideoHeightRequired,
		},
		{
			name: "videochannel requires fps",
			cfg: func() Config {
				cfg := base
				cfg.Transport = "videochannel"
				cfg.Video.Width = 640
				cfg.Video.Height = 480
				return cfg
			}(),
			want: ErrVideoFPSRequired,
		},
		{
			name: "tile codec requires square 1080 dimensions",
			cfg: func() Config {
				cfg := base
				cfg.Transport = "videochannel"
				cfg.Video.Width = 640
				cfg.Video.Height = 480
				cfg.Video.FPS = 30
				cfg.Video.Codec = "tile"
				return cfg
			}(),
			want: ErrTileCodecDimensions,
		},
		{
			name: "videochannel valid",
			cfg: func() Config {
				cfg := base
				cfg.Transport = "videochannel"
				cfg.Video.Width = 1080
				cfg.Video.Height = 1080
				cfg.Video.FPS = 30
				cfg.Video.Codec = "tile"
				return cfg
			}(),
		},
		{
			name: "vp8channel requires fps",
			cfg: func() Config {
				cfg := base
				cfg.Transport = "vp8channel"
				return cfg
			}(),
			want: ErrVP8FPSRequired,
		},
		{
			name: "vp8channel requires batch size",
			cfg: func() Config {
				cfg := base
				cfg.Transport = "vp8channel"
				cfg.VP8.FPS = 25
				return cfg
			}(),
			want: ErrVP8BatchSizeRequired,
		},
		{
			name: "vp8channel valid",
			cfg: func() Config {
				cfg := base
				cfg.Transport = "vp8channel"
				cfg.VP8.FPS = 25
				cfg.VP8.BatchSize = 16
				return cfg
			}(),
		},
		{
			name: "seichannel requires fps",
			cfg: func() Config {
				cfg := base
				cfg.Transport = "seichannel"
				return cfg
			}(),
			want: ErrSEIFPSRequired,
		},
		{
			name: "seichannel requires batch size",
			cfg: func() Config {
				cfg := base
				cfg.Transport = "seichannel"
				cfg.SEI.FPS = 20
				return cfg
			}(),
			want: ErrSEIBatchSizeRequired,
		},
		{
			name: "seichannel requires fragment size",
			cfg: func() Config {
				cfg := base
				cfg.Transport = "seichannel"
				cfg.SEI.FPS = 20
				cfg.SEI.BatchSize = 1
				return cfg
			}(),
			want: ErrSEIFragmentSizeRequired,
		},
		{
			name: "seichannel requires ack timeout",
			cfg: func() Config {
				cfg := base
				cfg.Transport = "seichannel"
				cfg.SEI.FPS = 20
				cfg.SEI.BatchSize = 1
				cfg.SEI.FragmentSize = 900
				return cfg
			}(),
			want: ErrSEIAckTimeoutRequired,
		},
		{
			name: "seichannel valid",
			cfg: func() Config {
				cfg := base
				cfg.Transport = "seichannel"
				cfg.SEI.FPS = 20
				cfg.SEI.BatchSize = 1
				cfg.SEI.FragmentSize = 900
				cfg.SEI.AckTimeoutMS = 3000
				return cfg
			}(),
		},
		{
			name: "cnc requires socks host",
			cfg: func() Config {
				cfg := base
				cfg.Mode = ModeCnc
				cfg.SOCKSPort = 1080
				return cfg
			}(),
			want: ErrSOCKSHostRequired,
		},
		{
			name: "cnc requires socks port",
			cfg: func() Config {
				cfg := base
				cfg.Mode = ModeCnc
				cfg.SOCKSHost = "127.0.0.1"
				return cfg
			}(),
			want: ErrSOCKSPortRequired,
		},
		{
			name: "cnc rejects unauthenticated wildcard socks bind",
			cfg: func() Config {
				cfg := base
				cfg.Mode = ModeCnc
				cfg.SOCKSHost = "0.0.0.0"
				cfg.SOCKSPort = 1080
				return cfg
			}(),
			want: ErrSOCKSAuthRequired,
		},
		{
			name: "cnc allows authenticated wildcard socks bind",
			cfg: func() Config {
				cfg := base
				cfg.Mode = ModeCnc
				cfg.SOCKSHost = "0.0.0.0"
				cfg.SOCKSPort = 1080
				cfg.SOCKSUser = "user"
				cfg.SOCKSPass = "pass"
				return cfg
			}(),
		},
		{
			name: "cnc allows localhost socks bind without auth",
			cfg: func() Config {
				cfg := base
				cfg.Mode = ModeCnc
				cfg.SOCKSHost = "localhost"
				cfg.SOCKSPort = 1080
				return cfg
			}(),
		},
		{
			name: "liveness rejects bad interval",
			cfg: func() Config {
				cfg := base
				cfg.LivenessInterval = testBadDuration
				return cfg
			}(),
			want: ErrLivenessIntervalInvalid,
		},
		{
			name: "liveness rejects zero timeout",
			cfg: func() Config {
				cfg := base
				cfg.LivenessTimeout = "0s"
				return cfg
			}(),
			want: ErrLivenessTimeoutInvalid,
		},
		{
			name: "liveness rejects negative failures",
			cfg: func() Config {
				cfg := base
				cfg.LivenessFailures = -1
				return cfg
			}(),
			want: ErrLivenessFailuresInvalid,
		},
		{
			name: "lifecycle accepts max session duration",
			cfg: func() Config {
				cfg := base
				cfg.MaxSessionDuration = "1h"
				return cfg
			}(),
		},
		{
			name: "lifecycle rejects bad max session duration",
			cfg: func() Config {
				cfg := base
				cfg.MaxSessionDuration = testBadDuration
				return cfg
			}(),
			want: ErrLifecycleMaxSessionDurationInvalid,
		},
		{
			name: "lifecycle rejects zero max session duration",
			cfg: func() Config {
				cfg := base
				cfg.MaxSessionDuration = "0s"
				return cfg
			}(),
			want: ErrLifecycleMaxSessionDurationInvalid,
		},
		{
			name: "traffic accepts shaping",
			cfg: func() Config {
				cfg := base
				cfg.TrafficMaxPayloadSize = 4096
				cfg.TrafficMinDelay = "5ms"
				cfg.TrafficMaxDelay = "30ms"
				return cfg
			}(),
		},
		{
			name: "traffic rejects negative max payload",
			cfg: func() Config {
				cfg := base
				cfg.TrafficMaxPayloadSize = -1
				return cfg
			}(),
			want: ErrTrafficMaxPayloadSizeInvalid,
		},
		{
			name: "traffic rejects payload too small for encrypted smux frame",
			cfg: func() Config {
				cfg := base
				cfg.TrafficMaxPayloadSize = runtime.MinSmuxWirePayload - 1
				return cfg
			}(),
			want: ErrTrafficMaxPayloadSizeInvalid,
		},
		{
			name: "traffic rejects bad min delay",
			cfg: func() Config {
				cfg := base
				cfg.TrafficMinDelay = testBadDuration
				return cfg
			}(),
			want: ErrTrafficMinDelayInvalid,
		},
		{
			name: "traffic rejects negative max delay",
			cfg: func() Config {
				cfg := base
				cfg.TrafficMaxDelay = "-1ms"
				return cfg
			}(),
			want: ErrTrafficMaxDelayInvalid,
		},
		{
			name: "traffic rejects max delay below min delay",
			cfg: func() Config {
				cfg := base
				cfg.TrafficMinDelay = "30ms"
				cfg.TrafficMaxDelay = "5ms"
				return cfg
			}(),
			want: ErrTrafficMaxDelayInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.cfg)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("Validate() error = %v, want %v", err, tt.want)
			}
		})
	}
}

const testProviderWBStream = "wbstream"

func TestValidateGen(t *testing.T) {
	RegisterDefaults()

	tests := []struct {
		name string
		cfg  Config
		want error
	}{
		{
			name: "custom resolver reaches provider validation",
			cfg: Config{
				Provider: testProviderWBStream, Resolver: &net.Resolver{PreferGo: true}, Amount: 3,
			},
			want: ErrUnsupportedProvider,
		},
		{
			name: "wbstream room generation unsupported",
			cfg:  Config{Provider: testProviderWBStream, DNSServer: "8.8.8.8:53", Amount: 3},
			want: ErrUnsupportedProvider,
		},
		{
			name: "missing provider",
			cfg:  Config{DNSServer: "8.8.8.8:53", Amount: 1},
			want: ErrProviderRequired,
		},
		{
			name: "unsupported provider",
			cfg:  Config{Provider: "unknown", DNSServer: "8.8.8.8:53", Amount: 1},
			want: ErrUnsupportedProvider,
		},
		{
			name: "missing dns",
			cfg:  Config{Provider: testProviderWBStream, Amount: 1},
			want: ErrDNSServerRequired,
		},
		{
			name: "amount zero",
			cfg:  Config{Provider: testProviderWBStream, DNSServer: "8.8.8.8:53", Amount: 0},
			want: ErrAmountRequired,
		},
		{
			name: "amount negative",
			cfg:  Config{Provider: testProviderWBStream, DNSServer: "8.8.8.8:53", Amount: -1},
			want: ErrAmountRequired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateGen(tt.cfg)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("ValidateGen() error = %v", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("ValidateGen() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestGenUnsupportedProvider(t *testing.T) {
	RegisterDefaults()
	cfg := Config{Provider: "telemost", DNSServer: "8.8.8.8:53", Amount: 1}
	err := Gen(context.Background(), cfg, func(string) {})
	if !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("Gen(telemost) error = %v, want ErrUnsupportedProvider", err)
	}
}
