package provider

import (
	"context"

	"github.com/pion/webrtc/v4"
)

type Provider interface {
	Connect(ctx context.Context) error
	Send(data []byte) error
	Close() error
	SetReconnectCallback(cb func(*webrtc.DataChannel))
	SetShouldReconnect(fn func() bool)
	SetEndedCallback(cb func(string))
	WatchConnection(ctx context.Context)
	CanSend() bool
	GetSendQueue() chan []byte
	GetBufferedAmount() uint64
}

type Config struct {
	RoomURL   string
	Name      string
	OnData    func([]byte)
	DNSServer string
	ProxyAddr string
	ProxyPort int
}

type Factory func(ctx context.Context, cfg Config) (Provider, error)

var providers = make(map[string]Factory)

func Register(name string, factory Factory) {
	providers[name] = factory
}

func New(ctx context.Context, name string, cfg Config) (Provider, error) {
	factory, ok := providers[name]
	if !ok {
		return nil, ErrProviderNotFound
	}
	return factory(ctx, cfg)
}

func Available() []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	return names
}
