// Package carrier exposes carrier-oriented registration and construction APIs.
package carrier

import (
	"context"

	"github.com/openlibrecommunity/olcrtc/internal/provider"
)

// Carrier is the current carrier implementation contract.
type Carrier = provider.Provider

// Config holds carrier configuration.
type Config = provider.Config

// Factory creates a new carrier instance.
type Factory = provider.Factory

// Register adds a carrier factory to the registry.
func Register(name string, factory Factory) {
	provider.Register(name, factory)
}

// New creates a carrier instance by name.
func New(ctx context.Context, name string, cfg Config) (Carrier, error) {
	return provider.New(ctx, name, cfg)
}

// Available returns a list of registered carriers.
func Available() []string {
	return provider.Available()
}
