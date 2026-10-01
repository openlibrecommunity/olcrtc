package protect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// ErrNoAddresses reports that a host resolved to no usable address.
var ErrNoAddresses = errors.New("host resolved to no address")

const (
	// familyFallbackDelay is how long the second address family waits before
	// racing the first, matching the stdlib Happy Eyeballs delay.
	familyFallbackDelay = 300 * time.Millisecond
	// routeProbeTimeout bounds the source-address probe, which sends nothing.
	routeProbeTimeout = time.Second
)

// familyDialError reports the failure of every address family that was tried.
//
// The stdlib returns only the primary family's error, so a v6 leg that died on
// its own - a protector that cannot pin an IPv6 socket, say - vanishes and
// leaves an unrelated "network is unreachable" from the v4 leg as the whole
// account of the failure. That cost a day of looking at the wrong half.
type familyDialError struct {
	address string
	ipv6    error
	ipv4    error
}

func (e *familyDialError) Error() string {
	parts := make([]string, 0, 2)
	if e.ipv6 != nil {
		parts = append(parts, "ipv6: "+e.ipv6.Error())
	}
	if e.ipv4 != nil {
		parts = append(parts, "ipv4: "+e.ipv4.Error())
	}
	return "dial " + e.address + ": " + strings.Join(parts, "; ")
}

func (e *familyDialError) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.ipv6 != nil {
		errs = append(errs, e.ipv6)
	}
	if e.ipv4 != nil {
		errs = append(errs, e.ipv4)
	}
	return errs
}

// familyResult carries one family's outcome back to the racer.
type familyResult struct {
	conn  net.Conn
	err   error
	first bool
}

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
	// A name that resolves to both families is dialed on both: the family
	// with a route first, the other racing familyFallbackDelay behind, so a
	// link without IPv4 still connects (App Review runs on IPv6-only NAT64).
	if network == "tcp" && len(v6) > 0 && len(v4) > 0 {
		return d.raceFamilies(ctx, address, port, v6, v4)
	}
	ips := familiesFor(network, v6, v4)
	if len(ips) == 0 {
		return nil, fmt.Errorf("dial failed: %w: no %s address for %s", ErrNoAddresses, network, host)
	}
	return d.dialAddrs(ctx, network, port, ips)
}

func (d *Dialer) raceFamilies(ctx context.Context, address, port string, v6, v4 []net.IP) (net.Conn, error) {
	first, second, firstIsV6 := orderFamilies(v6, v4)

	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan familyResult, 2)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		conn, err := d.dialAddrs(dialCtx, "tcp", port, first)
		results <- familyResult{conn: conn, err: err, first: true}
	}()
	go func() {
		timer := time.NewTimer(familyFallbackDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-firstDone:
		case <-dialCtx.Done():
		}
		conn, err := d.dialAddrs(dialCtx, "tcp", port, second)
		results <- familyResult{conn: conn, err: err, first: false}
	}()

	return collectRace(results, address, firstIsV6)
}

func collectRace(results chan familyResult, address string, firstIsV6 bool) (net.Conn, error) {
	var firstErr, secondErr error
	for received := range 2 {
		res := <-results
		if res.err == nil {
			go closeLate(results, 1-received)
			return res.conn, nil
		}
		if res.first {
			firstErr = res.err
		} else {
			secondErr = res.err
		}
	}
	if firstIsV6 {
		return nil, &familyDialError{address: address, ipv6: firstErr, ipv4: secondErr}
	}
	return nil, &familyDialError{address: address, ipv6: secondErr, ipv4: firstErr}
}

// closeLate drains the losing racer so a connection that lands after a winner
// was picked does not leak a socket.
func closeLate(results chan familyResult, count int) {
	for range count {
		if res := <-results; res.conn != nil {
			_ = res.conn.Close()
		}
	}
}

// orderFamilies puts the family that has a usable source address first.
//
// The probe runs on a protected socket, so it sees the interface the dial will
// really leave through rather than the process default route. Inside a VPN
// extension those are different interfaces, and the stdlib's own RFC 6724 probe
// only ever sees the latter - which is how a dial ends up preferring an A
// record on a link whose physical interface has no IPv4 at all.
// It returns the family to try first, the family to fall back to, and whether
// the first one is IPv6.
func orderFamilies(v6, v4 []net.IP) ([]net.IP, []net.IP, bool) {
	if !hasRoute(v6[0]) && hasRoute(v4[0]) {
		return v4, v6, false
	}
	return v6, v4, true
}

// hasRoute reports whether a protected socket can pick a source address for ip.
// Connecting a UDP socket assigns a route without sending anything.
func hasRoute(ip net.IP) bool {
	dialer := net.Dialer{Timeout: routeProbeTimeout, Control: controlFunc}
	conn, err := dialer.Dial("udp", net.JoinHostPort(ip.String(), defaultDNSPort))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
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
