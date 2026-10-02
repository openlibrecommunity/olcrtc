package protect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// ErrNoAddresses reports that a host resolved to no usable address.
var ErrNoAddresses = errors.New("host resolved to no address")

// Dialer dials protected sockets and resolves names through a Lookup, so a
// session's names go where its sockets go rather than to the system
// resolver. A nil Lookup leaves resolution to net.Dialer.
//
// ai-generated: replaces the fork's package-level DialContext.
type Dialer struct {
	lookup Lookup
}

// NewDialer returns a Dialer over the first non-nil lookup.
func NewDialer(lookups ...Lookup) *Dialer {
	return &Dialer{lookup: firstLookup(lookups)}
}

func firstLookup(lookups []Lookup) Lookup {
	for _, l := range lookups {
		if l != nil {
			return l
		}
	}
	return nil
}

func (d *Dialer) netDialer() *net.Dialer {
	return &net.Dialer{Timeout: defaultDialTimeout, KeepAlive: defaultKeepAlive, Control: controlFunc}
}

// Dial is DialContext with a background context.
func (d *Dialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

// DialContext resolves the host of address through the lookup and dials the
// addresses in order: IPv6 first, then IPv4. Literals dial directly.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d.lookup == nil || isIPLiteral(address) {
		conn, err := d.netDialer().DialContext(ctx, network, address)
		if err != nil {
			return nil, fmt.Errorf("dial failed: %w", err)
		}
		return conn, nil
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("dial failed: split address: %w", err)
	}
	v6, v4, err := lookupFamiliesWith(ctx, d.lookup, host)
	if err != nil {
		return nil, fmt.Errorf("dial failed: %w", err)
	}
	ips := familiesFor(network, v6, v4)
	if len(ips) == 0 {
		return nil, fmt.Errorf("dial failed: %w: no %s address for %s", ErrNoAddresses, network, host)
	}
	return d.dialAddrs(ctx, network, port, ips)
}

func (d *Dialer) dialAddrs(ctx context.Context, network, port string, ips []net.IP) (net.Conn, error) {
	var firstErr error
	for _, ip := range ips {
		conn, err := d.netDialer().DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("dial failed: %w", firstErr)
}

// familiesFor returns the addresses a network can use: one family when the
// network names it, both otherwise (IPv6 first).
func familiesFor(network string, v6, v4 []net.IP) []net.IP {
	switch {
	case strings.HasSuffix(network, "4"):
		return v4
	case strings.HasSuffix(network, "6"):
		return v6
	default:
		return append(append([]net.IP(nil), v6...), v4...)
	}
}

// lookupFamiliesWith asks a FamilyLookup once, or any Lookup once per family.
func lookupFamiliesWith(ctx context.Context, lookup Lookup, host string) ([]net.IP, []net.IP, error) {
	if fl, ok := lookup.(FamilyLookup); ok {
		v6, v4, err := fl.LookupFamilies(ctx, host)
		if err != nil {
			return nil, nil, fmt.Errorf("lookup families: %w", err)
		}
		return v6, v4, nil
	}
	v6, err6 := lookup.LookupIP(ctx, "ip6", host)
	v4, err4 := lookup.LookupIP(ctx, "ip4", host)
	if len(v6) == 0 && len(v4) == 0 {
		return nil, nil, firstNonNil(err6, err4)
	}
	return v6, v4, nil
}

// isIPLiteral reports whether address is host:port with an IP host.
func isIPLiteral(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	return net.ParseIP(host) != nil
}

// resolveAddress turns host:port into ip:port for one dial, picking the
// family the network names, else IPv6 when present.
func resolveAddress(ctx context.Context, lookup Lookup, network, address string) (string, error) {
	if isIPLiteral(address) || lookup == nil {
		return address, nil
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("split address: %w", err)
	}
	v6, v4, err := lookupFamiliesWith(ctx, lookup, host)
	if err != nil {
		return "", err
	}
	ips := familiesFor(network, v6, v4)
	if len(ips) == 0 {
		return "", fmt.Errorf("%w: no %s address for %s", ErrNoAddresses, network, host)
	}
	return net.JoinHostPort(ips[0].String(), port), nil
}
