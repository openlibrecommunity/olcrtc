package protect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// ErrDNSUnreachable reports that no configured DNS server could be reached.
var ErrDNSUnreachable = errors.New("no configured dns server could be reached")

// ai-generated: the fork's server ring, reshaped from process-wide state into
// a per-session Resolver (olcbox#13, #15, #16).

// Lookup resolves a host name to its addresses. *net.Resolver satisfies it,
// and so does *Resolver, which adds a server ring and a fallback to the
// host's own resolver.
type Lookup interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

// FamilyLookup is implemented by lookups that can answer both address
// families in one call. Dialer prefers it so a dual-stack dial costs one
// round of queries instead of two.
type FamilyLookup interface {
	LookupFamilies(ctx context.Context, host string) (v6, v4 []net.IP, err error)
}

const (
	defaultDNSPort    = "53"
	defaultDNSTimeout = 3 * time.Second
	// configuredLookupBudget bounds the whole attempt on the configured servers
	// before the host's resolver is asked. Without it a network that blackholes
	// every one of them costs the caller each server's silence in turn, and the
	// tunnel start it belongs to gives up first.
	configuredLookupBudget = 4 * time.Second
	// dnsQueryTimeout bounds how long one query waits for one server. Go gives
	// each exchange five seconds and then asks the same server again, so a
	// resolver a network has blackholed used to consume the whole lookup
	// budget in silence. A second and a half without an answer is already the
	// answer, and the ring moves on to the next server for the retry. It is
	// short enough for Go's two attempts to fit inside configuredLookupBudget
	// with room to spare, so both are counted before the budget cuts in.
	dnsQueryTimeout = 1500 * time.Millisecond
	// systemLookupBudget bounds the host's own resolver. Inside an iOS packet
	// tunnel that resolver points into the tunnel itself, where a query that
	// nothing serves would otherwise wait out the whole dial.
	systemLookupBudget = 5 * time.Second
	// ringDarkAfter is how many queries in a row go unanswered, with no
	// answer in between, before the configured servers count as dark: two
	// families' worth of silence and one more, which a single lookup on a
	// network that blackholes every public operator produces by itself.
	ringDarkAfter = 3
)

// ringDarkWindow is how long dark servers are left alone before a lookup asks
// them again - a network that blocked every public operator may have stopped.
const ringDarkWindow = 30 * time.Second

// The public operators the ring knows: each IPv4 anycast with its IPv6 twin.
const (
	cloudflareV4Addr = "1.1.1.1"
	cloudflareV6Addr = "2606:4700:4700::1111"
	googleV4Addr     = "8.8.8.8"
	googleV6Addr     = "2001:4860:4860::8888"
	quad9V4Addr      = "9.9.9.9"
	quad9V6Addr      = "2620:fe::fe"
)

// dnsIPv6Peer maps a public IPv4 resolver to the same operator's IPv6 address.
//
// Clients are configured with a single IPv4 literal - "1.1.1.1:53" on iOS,
// "8.8.8.8:53" by default here. On a link with no IPv4 that address cannot be
// reached at all, so resolution fails before anything dials, which is every
// session on an IPv6-only carrier and on the IPv6-only NAT64 network Apple runs
// App Review from. Falling back to the operator's own IPv6 address keeps the
// queries with the resolver that was already chosen, so no new party sees them.
var dnsIPv6Peer = map[string]string{ //nolint:gochecknoglobals // static lookup table, no state
	cloudflareV4Addr:  cloudflareV6Addr,
	"1.0.0.1":         "2606:4700:4700::1001",
	googleV4Addr:      googleV6Addr,
	"8.8.4.4":         "2001:4860:4860::8844",
	quad9V4Addr:       quad9V6Addr,
	"149.112.112.112": "2620:fe::9",
}

// publicOperators is the fallback order when the configured resolver is one of
// the public operators above and stays silent: a carrier that blocks 1.1.1.1
// outright is not a carrier with no resolvers, only one where that address is
// dead. Each entry is an operator's IPv4 anycast and its IPv6 twin. The list is
// consulted only when the configured server is itself a public operator -
// someone who chose a private resolver chose where their queries go, and a
// fallback would hand them to a party they never picked.
var publicOperators = [][2]string{ //nolint:gochecknoglobals // static lookup table, no state
	{cloudflareV4Addr, cloudflareV6Addr},
	{googleV4Addr, googleV6Addr},
	{quad9V4Addr, quad9V6Addr},
}

// Resolver asks the configured DNS servers over protected sockets, in
// order, and the host's own resolver when they answer nothing. Safe for
// concurrent use; SetServers replaces the list while lookups are running.
//
// It is not a rotation. While the configured server answers, every query goes
// to it - the AAAA beside the A, the second lookup after the first - because a
// fallback exists for failure, not for sharing every name with a second party.
// The host's resolver is the last resort: resolving names ourselves keeps a
// carrier from answering for a host it would rather we did not reach, but a
// network that blackholes every public operator leaves nothing at all, and no
// answer is worse than the carrier's.
type Resolver struct {
	mu   sync.RWMutex
	ring *serverRing
	// system is the host's resolver, asked when the ring answers nothing.
	// nil disables the fallback; tests point it at a fake server.
	system  *net.Resolver
	nowFunc func() time.Time
}

// NewResolver returns a resolver over servers, a list in the form
// SplitDNSServers accepts. An empty list yields a resolver that asks only
// the host.
func NewResolver(servers string) *Resolver {
	r := &Resolver{system: net.DefaultResolver, nowFunc: time.Now}
	r.SetServers(servers)
	return r
}

// SetServers replaces the server list. Lookups already in flight finish on
// the old ring; the next one asks the new servers first.
func (r *Resolver) SetServers(servers string) {
	// The ring reads the clock through the Resolver so a test can hold it still.
	ring := newServerRing(SplitDNSServers(servers), func() time.Time { return r.nowFunc() })
	r.mu.Lock()
	r.ring = ring
	r.mu.Unlock()
}

// Servers returns the expanded server list in the order it is asked.
func (r *Resolver) Servers() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.ring == nil {
		return nil
	}
	return append([]string(nil), r.ring.servers...)
}

func (r *Resolver) current() (*serverRing, *net.Resolver) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ring, r.system
}

// LookupIP implements Lookup. network is "ip", "ip4" or "ip6".
func (r *Resolver) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	v6, v4, err := r.LookupFamilies(ctx, host)
	if err != nil {
		return nil, err
	}
	switch network {
	case "ip4":
		return v4, nil
	case "ip6":
		return v6, nil
	default:
		return append(v6, v4...), nil
	}
}

// LookupFamilies resolves both address families and returns the IPv6
// addresses, the IPv4 addresses, and an error only when nothing answered.
//
// The configured servers are asked first and on a budget; if they produce no
// address at all the host's own resolver is asked, on a budget of its own.
// Servers that have just proven dark are asked last, since paying their
// silence again on every lookup buys nothing.
func (r *Resolver) LookupFamilies(ctx context.Context, host string) ([]net.IP, []net.IP, error) {
	if v6, v4, ok := literalFamilies(host); ok {
		return v6, v4, nil
	}
	ring, system := r.current()
	if ring == nil {
		return r.lookupHostOnly(ctx, system, host)
	}
	configured := ring.resolver()
	dark := system != nil && ring.dark(r.nowFunc())

	var configuredErr, systemErr error
	if !dark {
		v6, v4, err := lookupBounded(ctx, configured, host, configuredLookupBudget)
		if len(v6) > 0 || len(v4) > 0 {
			return v6, v4, nil
		}
		if system == nil {
			return nil, nil, &lookupError{host: host, configured: err}
		}
		configuredErr = err
	}
	v6, v4, err := lookupBounded(ctx, system, host, systemLookupBudget)
	if len(v6) > 0 || len(v4) > 0 {
		return v6, v4, nil
	}
	systemErr = err
	if dark {
		// The host's resolver had nothing either, so the configured servers
		// get their turn after all, on the usual budget.
		v6, v4, err = lookupBounded(ctx, configured, host, configuredLookupBudget)
		if len(v6) > 0 || len(v4) > 0 {
			return v6, v4, nil
		}
		configuredErr = err
	}
	return nil, nil, &lookupError{host: host, configured: configuredErr, system: systemErr}
}

func (r *Resolver) lookupHostOnly(ctx context.Context, system *net.Resolver, host string) ([]net.IP, []net.IP, error) {
	if system == nil {
		return nil, nil, &lookupError{host: host, configured: ErrDNSUnreachable}
	}
	v6, v4, err := lookupBounded(ctx, system, host, systemLookupBudget)
	if len(v6) > 0 || len(v4) > 0 {
		return v6, v4, nil
	}
	return nil, nil, &lookupError{host: host, system: err}
}

// literalFamilies answers an IP literal (zone allowed) without a lookup.
func literalFamilies(host string) ([]net.IP, []net.IP, bool) {
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, nil, false
	}
	if ip.To4() != nil {
		return nil, []net.IP{ip}, true
	}
	return []net.IP{ip}, nil, true
}

func newServerRing(servers []string, now func() time.Time) *serverRing {
	list := expandDNSServers(servers)
	if len(list) == 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &serverRing{servers: list, now: now}
}

// SplitDNSServers splits a list a platform hands over as one string - the
// servers of the network it stands on, separated by commas, semicolons or
// spaces - into its entries.
func SplitDNSServers(list string) []string {
	return strings.FieldsFunc(list, func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	})
}

// serverRing is the configured servers in preference order, with a cursor that
// moves past a server only once it has stayed silent on a query.
//
// It is not a rotation. While the configured server answers, every query goes
// to it - the AAAA beside the A, the second lookup after the first - because a
// fallback exists for failure, not for sharing every name with a second party.
//
// It also remembers silence. A network that blackholes every public operator
// does not change its mind between one lookup and the next, and a tunnel
// start makes several lookups in a row: once ringDarkAfter queries have gone
// unanswered with no answer between them the ring is dark, and lookupFamilies
// asks the host's resolver first instead of paying the silence again. After
// ringDarkWindow the ring is asked again (olcbox#16).
type serverRing struct {
	servers   []string
	now       func() time.Time
	preferred atomic.Int32
	// silences counts the queries that went unanswered since the last answer;
	// darkAt is when that count reached ringDarkAfter, zero while it has not.
	silences atomic.Int32
	darkAt   atomic.Int64
}

func (r *serverRing) resolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial:     r.dial,
	}
}

// dark reports whether the servers went unanswered recently enough for that
// to still be the expectation.
func (r *serverRing) dark(now time.Time) bool {
	at := r.darkAt.Load()
	if at == 0 {
		return false
	}
	if now.Sub(time.Unix(0, at)) <= ringDarkWindow {
		return true
	}
	// Time to ask again. The count starts over, so a ring that is still
	// silent goes dark again after one lookup's worth of silence.
	r.darkAt.Store(0)
	r.silences.Store(0)
	return false
}

func (r *serverRing) noteAnswer() {
	r.silences.Store(0)
	r.darkAt.Store(0)
}

func (r *serverRing) noteSilence(idx int) {
	r.demote(idx)
	if r.silences.Add(1) >= ringDarkAfter {
		r.darkAt.CompareAndSwap(0, r.now().UnixNano())
	}
}

// dial opens a query socket to the preferred server, or the next one that can
// be reached. Go's resolver passes the address from its own configuration and
// it is ignored: the servers here are the ones this process was told to use.
func (r *serverRing) dial(ctx context.Context, network, _ string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: defaultDNSTimeout, Control: controlFunc}
	count := len(r.servers)
	start := int(r.preferred.Load()) % count
	var firstErr error
	for i := range count {
		idx := (start + i) % count
		conn, err := dialer.DialContext(ctx, network, r.servers[idx])
		if err == nil {
			return wrapQuery(conn, r, idx), nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	if firstErr == nil {
		return nil, ErrDNSUnreachable
	}
	return nil, fmt.Errorf("%w: %w", ErrDNSUnreachable, firstErr)
}

// demote moves the cursor past a server that stayed silent - once. Two queries
// timing out on the same server together must not skip the one after it.
func (r *serverRing) demote(idx int) {
	//nolint:gosec // G115: an index into a short list
	r.preferred.CompareAndSwap(int32(idx), int32((idx+1)%len(r.servers)))
}

// queryConn gives the server a bounded time to answer each read, and tells the
// ring when it did not. Go's resolver sets its own, longer deadline on the
// socket; the shorter one here is what turns silence into the next server.
type queryConn struct {
	net.Conn
	ring *serverRing
	idx  int
}

func (c *queryConn) Read(p []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(dnsQueryTimeout))
	n, err := c.Conn.Read(p)
	c.note(n, err)
	return n, err //nolint:wrapcheck // Go's resolver classifies the raw conn error
}

// note tells the ring how the read went: an answer, or the silence that moves
// the cursor on and, repeated, turns the ring dark. A query still waiting when
// the lookup's budget closed the socket went unanswered too.
func (c *queryConn) note(n int, err error) {
	if err == nil && n > 0 {
		c.ring.noteAnswer()
		return
	}
	var netErr net.Error
	if (errors.As(err, &netErr) && netErr.Timeout()) || errors.Is(err, net.ErrClosed) {
		c.ring.noteSilence(c.idx)
	}
}

// packetQueryConn keeps a UDP query socket a net.PacketConn: Go's resolver
// checks for that interface and would otherwise frame the query for a stream.
type packetQueryConn struct {
	*queryConn
	packet net.PacketConn
}

func (c *packetQueryConn) ReadFrom(p []byte) (int, net.Addr, error) {
	_ = c.packet.SetReadDeadline(time.Now().Add(dnsQueryTimeout))
	n, addr, err := c.packet.ReadFrom(p)
	c.note(n, err)
	return n, addr, err //nolint:wrapcheck // Go's resolver classifies the raw conn error
}

func (c *packetQueryConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	return c.packet.WriteTo(p, addr) //nolint:wrapcheck // Go's resolver classifies the raw conn error
}

func wrapQuery(conn net.Conn, ring *serverRing, idx int) net.Conn {
	q := &queryConn{Conn: conn, ring: ring, idx: idx}
	if packet, ok := conn.(net.PacketConn); ok {
		return &packetQueryConn{queryConn: q, packet: packet}
	}
	return q
}

// expandDNSServers normalizes each server and appends the IPv6 address of every
// known IPv4 public resolver, dropping duplicates and keeping the given order.
func expandDNSServers(servers []string) []string {
	out := make([]string, 0, len(servers)*2)
	seen := make(map[string]struct{}, len(servers)*2)
	add := func(addr string) {
		if _, dup := seen[addr]; dup {
			return
		}
		seen[addr] = struct{}{}
		out = append(out, addr)
	}
	publicPort := ""
	for _, entry := range servers {
		for _, server := range SplitDNSServers(entry) {
			for _, addr := range withIPv6Peer(server) {
				add(addr)
			}
			if host, port, err := net.SplitHostPort(normalizeDNSServer(server)); err == nil {
				if _, public := dnsIPv6Peer[host]; public && publicPort == "" {
					publicPort = port
				}
			}
		}
	}
	// The other operators, in their fixed order, behind whatever was
	// configured - and only when what was configured is one of them.
	if publicPort != "" {
		for _, operator := range publicOperators {
			add(net.JoinHostPort(operator[0], publicPort))
			add(net.JoinHostPort(operator[1], publicPort))
		}
	}
	return out
}

func withIPv6Peer(server string) []string {
	addr := normalizeDNSServer(server)
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return []string{addr}
	}
	peer, ok := dnsIPv6Peer[host]
	if !ok {
		return []string{addr}
	}
	return []string{addr, net.JoinHostPort(peer, port)}
}

// normalizeDNSServer accepts "host", "host:port" and "[v6]:port".
func normalizeDNSServer(server string) string {
	trimmed := strings.TrimSpace(server)
	if trimmed == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(trimmed); err == nil {
		return trimmed
	}
	return net.JoinHostPort(strings.Trim(trimmed, "[]"), defaultDNSPort)
}

// lookupError is what a lookup nobody answered says: which resolver was asked
// and what each one said. "8.8.8.8 timed out" and "the carrier's resolver knows
// no such host" look the same in a log and mean different things (olcbox#16).
type lookupError struct {
	host       string
	configured error
	system     error
}

func (e *lookupError) Error() string {
	parts := make([]string, 0, 2)
	if e.configured != nil {
		parts = append(parts, "configured servers: "+e.configured.Error())
	}
	if e.system != nil {
		parts = append(parts, "host resolver: "+e.system.Error())
	}
	return "lookup " + e.host + ": " + strings.Join(parts, "; ")
}

func (e *lookupError) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.configured != nil {
		errs = append(errs, e.configured)
	}
	if e.system != nil {
		errs = append(errs, e.system)
	}
	return errs
}

// lookupBounded is lookupBoth on a budget of its own within ctx.
func lookupBounded(
	ctx context.Context, r *net.Resolver, host string, budget time.Duration,
) ([]net.IP, []net.IP, error) {
	bounded, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return lookupBoth(bounded, r, host)
}

// lookupBoth resolves both address families through one resolver, concurrently.
func lookupBoth(ctx context.Context, r *net.Resolver, host string) ([]net.IP, []net.IP, error) {
	type answer struct {
		ips []net.IP
		err error
	}
	ch6, ch4 := make(chan answer, 1), make(chan answer, 1)
	go func() {
		ips, lookupErr := lookupFamily(ctx, r, "ip6", host)
		ch6 <- answer{ips: ips, err: lookupErr}
	}()
	go func() {
		ips, lookupErr := lookupFamily(ctx, r, "ip4", host)
		ch4 <- answer{ips: ips, err: lookupErr}
	}()
	got6, got4 := <-ch6, <-ch4

	if len(got6.ips) == 0 && len(got4.ips) == 0 {
		return nil, nil, firstNonNil(got6.err, got4.err)
	}
	return got6.ips, got4.ips, nil
}

// lookupFamily resolves one address family on its own.
//
// Asking per family rather than for "ip" is what keeps one family's failure off
// the other: the combined lookup returns whichever family answered as if it
// were the whole answer, so the caller cannot tell a host that has no AAAA from
// one whose AAAA query failed, and either way is handed a list with nothing to
// fall back to.
func lookupFamily(ctx context.Context, r *net.Resolver, network, host string) ([]net.IP, error) {
	ips, err := r.LookupIP(ctx, network, host)
	if err != nil {
		return nil, fmt.Errorf("lookup %s: %w", network, err)
	}
	return ips, nil
}

func firstNonNil(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return ErrNoAddresses
}
