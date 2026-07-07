// Package dnsutil provides resolver helpers for single- and multi-endpoint DNS
// configs used by olcrtc runtime components.
package dnsutil

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// NewResolver returns a Go resolver that dials one or more comma-separated DNS
// servers in order. A single server preserves the previous behavior.
func NewResolver(servers string, timeout time.Duration) *net.Resolver {
	addrs := SplitServers(servers)
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialServer(ctx, network, addrs, timeout)
		},
	}
}

// SplitServers parses net.dns values like "8.8.8.8:53,192.168.1.1:53".
func SplitServers(servers string) []string {
	var out []string
	for _, part := range strings.Split(servers, ",") {
		addr := strings.TrimSpace(part)
		if addr != "" {
			out = append(out, addr)
		}
	}
	return out
}

func dialServer(ctx context.Context, network string, servers []string, timeout time.Duration) (net.Conn, error) {
	if len(servers) == 0 {
		return nil, fmt.Errorf("dns resolver has no servers")
	}
	network = resolverNetwork(network)
	var lastErr error
	for _, server := range servers {
		dialCtx := ctx
		cancel := func() {}
		if timeout > 0 {
			dialCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		conn, err := (&net.Dialer{}).DialContext(dialCtx, network, server)
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("dns dial failed via %s: %w", strings.Join(servers, ","), lastErr)
}

func resolverNetwork(network string) string {
	return network
}
