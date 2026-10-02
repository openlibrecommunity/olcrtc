// Package builtin wires the built-in auth providers to their engines and
// registers a name-keyed factory that transports use to obtain an
// [engine.Session]. When the auth provider is "none" the caller supplies
// engine/URL/token directly; otherwise the named provider issues credentials
// and the engine it reports is constructed.
package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/openlibrecommunity/olcrtc/internal/auth"
	authJitsi "github.com/openlibrecommunity/olcrtc/internal/auth/jitsi"
	authTelemost "github.com/openlibrecommunity/olcrtc/internal/auth/telemost"
	authWBStream "github.com/openlibrecommunity/olcrtc/internal/auth/wbstream"
	"github.com/openlibrecommunity/olcrtc/internal/engine"
	"github.com/openlibrecommunity/olcrtc/internal/engine/goolom"
	engineJitsi "github.com/openlibrecommunity/olcrtc/internal/engine/jitsi"
	"github.com/openlibrecommunity/olcrtc/internal/engine/livekit"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

// defaultDirectEngine is used by the "none" provider when the config does not
// name an engine explicitly.
const defaultDirectEngine = "livekit"

// ErrProviderNotFound is returned when an unregistered provider name is requested.
var ErrProviderNotFound = errors.New("auth provider not found")

// ErrAuthFailed wraps an auth provider rejection. It pairs with the inner
// provider error returned from [Open].
var ErrAuthFailed = errors.New("auth provider rejected the request")

// Config holds the inputs to [Open]. The fields mirror the subset of
// transport.Config that engines consume.
type Config struct {
	RoomURL             string
	Name                string
	OnData              func([]byte)
	OnPeerData          func(peerID string, data []byte)
	DNSServer           string
	Resolver            protect.Lookup
	ProxyAddr           string
	ProxyPort           int
	RequireTargetedPeer bool
	// Engine, URL, Token are honoured only for the "none" provider (direct
	// engine access); other providers derive them from their auth flow.
	Engine string
	URL    string
	Token  string
	// ProviderToken is an optional pre-issued account token forwarded to the auth
	// provider so it can act as that account instead of running its guest
	// flow (e.g. a WB Stream account token). Empty uses the guest flow.
	ProviderToken string
}

// Factory creates an engine session for a given provider.
type Factory func(ctx context.Context, cfg Config) (engine.Session, error)

//nolint:gochecknoglobals // process-wide provider registry
var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register adds a provider factory.
func Register(name string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()

	registry[name] = f
}

// Open looks up the provider factory and creates an engine session.
func Open(ctx context.Context, name string, cfg Config) (engine.Session, error) {
	registryMu.RLock()
	factory, ok := registry[name]
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrProviderNotFound, name)
	}

	return factory(ctx, cfg)
}

// Available reports all registered provider names, sorted.
func Available() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

// RegisterDefaults wires the built-in providers: jitsi, telemost, wbstream
// and "none" (direct engine access).
func RegisterDefaults() {
	engine.Register("livekit", livekit.New)
	engine.Register("goolom", goolom.New)
	engine.Register("jitsi", engineJitsi.New)
	register("wbstream", authWBStream.Provider{})
	register("telemost", authTelemost.Provider{})
	register("jitsi", authJitsi.Provider{})
	register("none", nil)
}

// register adds a provider factory. A nil provider means direct engine access:
// the caller supplies engine/URL/token through [Config] and no auth flow runs.
func register(name string, provider auth.Provider) {
	if provider != nil {
		auth.Register(name, provider)
	}
	Register(name, func(ctx context.Context, cfg Config) (engine.Session, error) {
		engineName, creds, refresh, err := resolveCredentials(ctx, provider, cfg)
		if err != nil {
			return nil, err
		}

		sess, err := engine.New(ctx, engineName, engine.Config{
			URL:                 creds.URL,
			Token:               creds.Token,
			Extra:               creds.Extra,
			Name:                cfg.Name,
			OnData:              cfg.OnData,
			OnPeerData:          cfg.OnPeerData,
			DNSServer:           cfg.DNSServer,
			Resolver:            cfg.Resolver,
			ProxyAddr:           cfg.ProxyAddr,
			ProxyPort:           cfg.ProxyPort,
			RequireTargetedPeer: cfg.RequireTargetedPeer,
			Refresh:             refresh,
		})
		if err != nil {
			return nil, fmt.Errorf("engine new: %w", err)
		}

		return sess, nil
	})
}

// resolveCredentials returns the engine to build, the credentials to build it
// with and, for auth-backed providers, a callback that re-issues them when the
// engine needs to reconnect with fresh ones.
func resolveCredentials(
	ctx context.Context,
	provider auth.Provider,
	cfg Config,
) (string, engine.Credentials, func(context.Context) (engine.Credentials, error), error) {
	if provider == nil {
		engineName := cfg.Engine
		if engineName == "" {
			engineName = defaultDirectEngine
		}

		return engineName, engine.Credentials{URL: cfg.URL, Token: cfg.Token}, nil, nil
	}

	authCfg := auth.Config{
		RoomURL:   cfg.RoomURL,
		Name:      cfg.Name,
		Token:     cfg.ProviderToken,
		DNSServer: cfg.DNSServer,
		Resolver:  cfg.Resolver,
		ProxyAddr: cfg.ProxyAddr,
		ProxyPort: cfg.ProxyPort,
	}

	issue := func(ctx context.Context) (engine.Credentials, error) {
		creds, err := provider.Issue(ctx, authCfg)
		if err != nil {
			return engine.Credentials{}, fmt.Errorf("%w: %w", ErrAuthFailed, err)
		}

		return engine.Credentials{URL: creds.URL, Token: creds.Token, Extra: creds.Extra}, nil
	}

	creds, err := issue(ctx)
	if err != nil {
		return "", engine.Credentials{}, nil, err
	}

	return provider.Engine(), creds, issue, nil
}
