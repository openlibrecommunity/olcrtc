// Package auth defines how room credentials are produced for an engine.
//
// An auth provider is responsible for any service-specific HTTP / login flow
// (WB Stream, Yandex Telemost, Jitsi, ...) and produces a
// Credentials value that an engine can use to connect. Some auth providers
// also support creating new rooms; that capability is optional and is
// expressed via the RoomCreator interface.
//
// The "none" auth provider passes a caller-supplied URL+Token through
// unchanged - this is the path that sing-box and other downstream consumers
// take when they want to use olcrtc as a generic LiveKit/Goolom/Jitsi
// transport without any service-specific behaviour baked in.
package auth

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

var (
	// ErrProviderNotFound is returned when a requested provider is not registered.
	ErrProviderNotFound = errors.New("provider not found")
	// ErrRoomIDRequired is returned when an auth flow needs an existing room ID and none was supplied.
	ErrRoomIDRequired = errors.New("room ID required")
)

// Credentials carry everything an engine needs to connect to an SFU.
//
// URL is the signaling endpoint (e.g. wss://livekit.example/). Token is the
// access token (LiveKit JWT, Goolom session credential, etc). Extra is for
// engine-specific bits that don't fit the common shape - engines should not
// rely on it being populated unless their paired auth provider documents it.
type Credentials struct {
	URL   string
	Token string
	Extra map[string]string
}

// Config is the input to an auth provider.
type Config struct {
	// RoomURL is the user-facing room link (e.g. https://telemost.yandex.ru/j/123).
	// Optional for providers that can also create rooms on demand.
	RoomURL string
	// Name is the display name to register with.
	Name string
	// Token is an optional pre-issued account token. When set, a provider may
	// skip its anonymous/guest auth flow and act as that account instead.
	// Empty means the provider falls back to its default (guest) flow.
	Token string
	// DNSServer / ProxyAddr / ProxyPort are network knobs for outbound HTTP.
	DNSServer string
	Resolver  protect.Lookup
	ProxyAddr string
	ProxyPort int
}

// Provider produces engine credentials.
type Provider interface {
	// Engine reports which engine this auth provider feeds.
	Engine() string
	// DefaultServiceURL returns the well-known service URL for this provider
	// (e.g. "https://stream.wb.ru"). Returns "" if no default exists - in that
	// case the caller must supply -url explicitly.
	DefaultServiceURL() string
	// Issue obtains credentials for the given room.
	Issue(ctx context.Context, cfg Config) (Credentials, error)
}

// RoomCreator is implemented by auth providers that can create new rooms
// against their backing service. Used by `olcrtc -mode gen`.
type RoomCreator interface {
	CreateRoom(ctx context.Context, cfg Config) (roomID string, err error)
}

//nolint:gochecknoglobals // process-wide auth provider registry
var (
	registryMu sync.RWMutex
	registry   = make(map[string]Provider)
)

// Register adds an auth provider to the registry.
func Register(name string, p Provider) {
	registryMu.Lock()
	defer registryMu.Unlock()

	registry[name] = p
}

// Get returns a registered auth provider by name.
func Get(name string) (Provider, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	p, ok := registry[name]
	if !ok {
		return nil, ErrProviderNotFound
	}
	return p, nil
}

// Available returns the list of registered auth provider names.
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

// RoomCreators returns the sorted names of registered providers that
// implement RoomCreator, i.e. the ones `-mode gen` can drive. The list is
// empty when no provider can create rooms.
func RoomCreators() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	names := make([]string, 0, len(registry))
	for name, p := range registry {
		if _, ok := p.(RoomCreator); ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
