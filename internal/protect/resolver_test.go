package protect

import (
	"context"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/fakedns"
)

// ai-generated: whole file, unit cover for the per-session resolver ring.

// cloudflareV4 is the literal every iOS build is configured with.
const cloudflareV4 = "1.1.1.1:53"

// newTestResolver builds a Resolver whose host fallback is a fakedns server
// (or none), so no test ever asks the real system resolver.
func newTestResolver(t *testing.T, servers string, system *fakedns.Server) *Resolver {
	t.Helper()
	r := NewResolver(servers)
	if system == nil {
		r.system = nil
		return r
	}
	r.system = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, system.Addr)
	}}
	return r
}

func startDNS(t *testing.T, records map[string]string) *fakedns.Server {
	t.Helper()
	srv, err := fakedns.Start(records)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func startSilentDNS(t *testing.T) *fakedns.Server {
	t.Helper()
	srv, err := fakedns.StartSilent()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func lookupV4(t *testing.T, r *Resolver, host string, budget time.Duration) ([]net.IP, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	return r.LookupIP(ctx, "ip4", host)
}

func TestExpandDNSServersAddsTheOperatorsIPv6Address(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			// The iOS client configures exactly this, and on a link without
			// IPv4 it is unreachable on its own.
			name: "cloudflare ipv4 literal gains its ipv6 address",
			in:   []string{cloudflareV4},
			want: []string{
				cloudflareV4, "[2606:4700:4700::1111]:53",
				"8.8.8.8:53", "[2001:4860:4860::8888]:53",
				"9.9.9.9:53", "[2620:fe::fe]:53",
			},
		},
		{
			name: "bare address gets the default port",
			in:   []string{"8.8.8.8"},
			want: []string{
				"8.8.8.8:53", "[2001:4860:4860::8888]:53",
				"1.1.1.1:53", "[2606:4700:4700::1111]:53",
				"9.9.9.9:53", "[2620:fe::fe]:53",
			},
		},
		{
			name: "ipv6 literal is bracketed and left alone",
			in:   []string{"2620:fe::fe"},
			want: []string{"[2620:fe::fe]:53"},
		},
		{
			name: "unknown resolver keeps whatever was configured",
			in:   []string{"resolver.example:5353"},
			want: []string{"resolver.example:5353"},
		},
		{
			name: "duplicates collapse and order is kept",
			in:   []string{"1.1.1.1", cloudflareV4, "9.9.9.9"},
			want: []string{
				cloudflareV4, "[2606:4700:4700::1111]:53", "9.9.9.9:53", "[2620:fe::fe]:53",
				"8.8.8.8:53", "[2001:4860:4860::8888]:53",
			},
		},
		{
			name: "blank entries are dropped",
			in:   []string{"", "   ", "1.0.0.1:53"},
			want: []string{
				"1.0.0.1:53", "[2606:4700:4700::1001]:53",
				"1.1.1.1:53", "[2606:4700:4700::1111]:53",
				"8.8.8.8:53", "[2001:4860:4860::8888]:53",
				"9.9.9.9:53", "[2620:fe::fe]:53",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expandDNSServers(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("expandDNSServers(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for _, addr := range got {
				if _, _, err := net.SplitHostPort(addr); err != nil {
					t.Fatalf("%q is not dialable: %v", addr, err)
				}
			}
		})
	}
}

// A public resolver that a network blocks needs another public resolver behind
// it, so the operators the engine already knows are appended - in a fixed
// order, each with its IPv6 twin. A private resolver gets none of them: whoever
// configured it chose where their queries go.
func TestExpandDNSServersAppendsTheOtherPublicOperators(t *testing.T) {
	cases := map[string]struct {
		in   []string
		want []string
	}{
		"cloudflare first": {
			in: []string{"1.1.1.1:53"},
			want: []string{
				"1.1.1.1:53", "[2606:4700:4700::1111]:53",
				"8.8.8.8:53", "[2001:4860:4860::8888]:53",
				"9.9.9.9:53", "[2620:fe::fe]:53",
			},
		},
		"google first": {
			in: []string{"8.8.8.8:53"},
			want: []string{
				"8.8.8.8:53", "[2001:4860:4860::8888]:53",
				"1.1.1.1:53", "[2606:4700:4700::1111]:53",
				"9.9.9.9:53", "[2620:fe::fe]:53",
			},
		},
		"a private resolver stays alone": {
			in:   []string{"10.0.0.1:53"},
			want: []string{"10.0.0.1:53"},
		},
		"the platform's list stays ahead of the operators": {
			in: []string{"10.0.0.1:53, fe80::1%pdp_ip0 1.1.1.1:53"},
			want: []string{
				"10.0.0.1:53", "[fe80::1%pdp_ip0]:53",
				"1.1.1.1:53", "[2606:4700:4700::1111]:53",
				"8.8.8.8:53", "[2001:4860:4860::8888]:53",
				"9.9.9.9:53", "[2620:fe::fe]:53",
			},
		},
	}
	for name, tc := range cases {
		got := expandDNSServers(tc.in)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s: expandDNSServers(%v)\n got  %v\n want %v", name, tc.in, got, tc.want)
		}
	}
}

func TestSplitDNSServersAcceptsWhatPlatformsHandOver(t *testing.T) {
	got := SplitDNSServers(" 10.0.0.1:53, fe80::1%pdp_ip0 ;1.1.1.1 ")
	want := []string{"10.0.0.1:53", "fe80::1%pdp_ip0", "1.1.1.1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("SplitDNSServers() = %q, want %q", got, want)
	}
	if got := SplitDNSServers(" , ; "); len(got) != 0 {
		t.Fatalf("SplitDNSServers() of separators alone = %q, want nothing", got)
	}
}

func TestResolverWithoutServersAsksOnlyTheHost(t *testing.T) {
	host := startDNS(t, map[string]string{"carrier.test": "10.0.0.9"})
	r := newTestResolver(t, "", host)
	if r.Servers() != nil {
		t.Fatalf("Servers() = %v, want none", r.Servers())
	}
	ips, err := lookupV4(t, r, "carrier.test", 10*time.Second)
	if err != nil || len(ips) != 1 || ips[0].String() != "10.0.0.9" {
		t.Fatalf("LookupIP = %v, %v; want the host resolver's answer", ips, err)
	}
	blind := newTestResolver(t, "", nil)
	if _, err := lookupV4(t, blind, "carrier.test", time.Second); err == nil {
		t.Fatal("a resolver with neither servers nor a host resolver answered")
	}
}

func TestSetServersReplacesTheListInPlace(t *testing.T) {
	first := startDNS(t, map[string]string{"carrier.test": "10.0.0.1"})
	second := startDNS(t, map[string]string{"carrier.test": "10.0.0.2"})

	r := newTestResolver(t, first.Addr, nil)
	r.SetServers(second.Addr)
	ips, err := lookupV4(t, r, "carrier.test", 10*time.Second)
	if err != nil || len(ips) != 1 || ips[0].String() != "10.0.0.2" {
		t.Fatalf("LookupIP after SetServers = %v, %v; want the new server's answer", ips, err)
	}
	if got := r.Servers(); len(got) != 1 || got[0] != second.Addr {
		t.Fatalf("Servers() = %v, want [%s]", got, second.Addr)
	}
	if first.Queries() != 0 {
		t.Fatalf("the replaced server was asked %d times", first.Queries())
	}
}

func TestLookupIPAnswersLiteralsWithoutAsking(t *testing.T) {
	dns := startDNS(t, map[string]string{})
	r := newTestResolver(t, dns.Addr, nil)
	ips, err := lookupV4(t, r, "192.0.2.7", time.Second)
	if err != nil || len(ips) != 1 || ips[0].String() != "192.0.2.7" {
		t.Fatalf("LookupIP(literal) = %v, %v", ips, err)
	}
	v6, v4, err := r.LookupFamilies(context.Background(), "fe80::1%lo")
	if err != nil || len(v6) != 1 || len(v4) != 0 {
		t.Fatalf("LookupFamilies(zoned literal) = %v, %v, %v", v6, v4, err)
	}
	if dns.Queries() != 0 {
		t.Fatalf("a literal cost %d queries", dns.Queries())
	}
}

// A resolver that a carrier has blackholed is routable: the UDP connect that
// picks a server "succeeds" against it every time, so the ring never moved on
// and every attempt Go made went to the same silence. The next server is asked
// once the first has had a bounded time to answer and has not.
func TestResolverMovesOnWhenTheConfiguredServerIsSilent(t *testing.T) {
	silent := startSilentDNS(t)
	answering := startDNS(t, map[string]string{"carrier.test": "192.0.2.10"})

	r := newTestResolver(t, silent.Addr+", "+answering.Addr, nil)
	started := time.Now()
	ips, err := lookupV4(t, r, "carrier.test", 8*time.Second)
	if err != nil {
		t.Fatalf("LookupIP() = %v after %v, want the answer from the second server", err, time.Since(started))
	}
	if len(ips) != 1 || ips[0].String() != "192.0.2.10" {
		t.Fatalf("LookupIP() = %v, want [192.0.2.10]", ips)
	}
	if silent.Queries() == 0 {
		t.Fatal("the configured server was never asked; it must come first")
	}
	if elapsed := time.Since(started); elapsed > 6*time.Second {
		t.Fatalf("took %v; a silent server should cost about one query timeout, not Go's whole retry budget", elapsed)
	}
}

// The other side of moving on: while the configured server answers, nobody
// else is asked - not for AAAA beside A, not on the second lookup. Fallback
// is for failure, not a rotation that shares every name with a second party.
func TestResolverStaysWithTheConfiguredServerWhileItAnswers(t *testing.T) {
	first := startDNS(t, map[string]string{"carrier.test": "192.0.2.10"})
	second := startDNS(t, map[string]string{"carrier.test": "192.0.2.20"})

	r := newTestResolver(t, first.Addr+","+second.Addr, nil)
	for range 3 {
		ips, err := lookupV4(t, r, "carrier.test", 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if len(ips) != 1 || ips[0].String() != "192.0.2.10" {
			t.Fatalf("LookupIP() = %v, want the first server's answer", ips)
		}
	}
	if n := second.Queries(); n != 0 {
		t.Fatalf("second server was asked %d times while the first answered", n)
	}
}

// The host's own resolver is the last resort, and it has to be, because the
// configured one is a public operator and some networks blackhole every public
// operator there is. Answering nothing then is worse than asking the carrier:
// before olcRTC resolved names itself the carrier's resolver was all there was,
// and on the networks that permit it, it works (olcbox#15).
func TestResolverFallsBackToTheHostWhenNoConfiguredServerAnswers(t *testing.T) {
	silent := startSilentDNS(t)
	host := startDNS(t, map[string]string{"carrier.test": "192.0.2.30"})

	r := newTestResolver(t, silent.Addr, host)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	v6, v4, err := r.LookupFamilies(ctx, "carrier.test")
	if err != nil {
		t.Fatalf("LookupFamilies() = %v after %v, want the host resolver's answer", err, time.Since(started))
	}
	if len(v4) != 1 || v4[0].String() != "192.0.2.30" {
		t.Fatalf("LookupFamilies() v4 = %v (v6 %v), want [192.0.2.30]", v4, v6)
	}
	if silent.Queries() == 0 {
		t.Fatal("the configured server was never asked; it must come first")
	}
}

// And the configured server keeps its priority: while it answers, the host's
// resolver is never consulted, because the whole point of resolving names
// ourselves is not to ask a carrier that may lie about them.
func TestConfiguredServerIsPreferredOverTheHostResolver(t *testing.T) {
	configured := startDNS(t, map[string]string{"carrier.test": "192.0.2.10"})
	host := startDNS(t, map[string]string{"carrier.test": "192.0.2.30"})

	r := newTestResolver(t, configured.Addr, host)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, v4, err := r.LookupFamilies(ctx, "carrier.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(v4) != 1 || v4[0].String() != "192.0.2.10" {
		t.Fatalf("LookupFamilies() v4 = %v, want the configured server's [192.0.2.10]", v4)
	}
	if n := host.Queries(); n != 0 {
		t.Fatalf("the host's resolver was asked %d times while the configured one answered", n)
	}
}

// A carrier that blackholes every public operator does not change its mind
// between one lookup and the next, so paying the configured servers' silence
// on every lookup is paying for what is already known - and a tunnel start
// makes several lookups in a row, each of which used to cost the whole
// budget. The first lookup learns it; the ones that follow go to the host's
// resolver at once (olcbox#16).
func TestLookupSkipsTheConfiguredServersWhileTheyAllStaySilent(t *testing.T) {
	silent := startSilentDNS(t)
	host := startDNS(t, map[string]string{"carrier.test": "192.0.2.30"})

	r := newTestResolver(t, silent.Addr, host)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, _, err := r.LookupFamilies(ctx, "carrier.test"); err != nil {
		t.Fatalf("first LookupFamilies() = %v, want the host's answer", err)
	}
	asked := silent.Queries()
	if asked == 0 {
		t.Fatal("the configured server was never asked on the first lookup; it must come first")
	}

	started := time.Now()
	_, v4, err := r.LookupFamilies(ctx, "carrier.test")
	if err != nil {
		t.Fatalf("second LookupFamilies() = %v, want the host's answer", err)
	}
	if len(v4) != 1 || v4[0].String() != "192.0.2.30" {
		t.Fatalf("second LookupFamilies() v4 = %v, want [192.0.2.30]", v4)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("second lookup took %v; the configured servers' silence was already known", elapsed)
	}
	if n := silent.Queries(); n != asked {
		t.Fatalf("the silent server was asked %d more times on the second lookup", n-asked)
	}
}

// When the host's resolver fails as well, the error used to name only the
// configured servers' timeout, so a log read as "8.8.8.8 is blocked" when the
// carrier's own resolver had refused the name too - the two look the same and
// mean different things. Both halves are named.
func TestLookupErrorNamesBothResolvers(t *testing.T) {
	silent := startSilentDNS(t)
	// Answers, but knows no names: the "no such host" a filtering resolver gives.
	host := startDNS(t, map[string]string{})

	r := newTestResolver(t, silent.Addr, host)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _, err := r.LookupFamilies(ctx, "carrier.test")
	if err == nil {
		t.Fatal("LookupFamilies() succeeded with nobody knowing the name")
	}
	msg := err.Error()
	for _, want := range []string{"configured", "i/o timeout", "host", "no such host"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

// Dark is not forever: a network that blocked every public operator may stop,
// and a phone moves between networks under a running tunnel. Once the window
// has passed the configured servers are asked first again.
func TestLookupAsksTheConfiguredServersAgainAfterTheDarkWindow(t *testing.T) {
	silent := startSilentDNS(t)
	host := startDNS(t, map[string]string{"carrier.test": "192.0.2.30"})

	r := newTestResolver(t, silent.Addr, host)
	// The ring's clock, held still: the window is measured on it, not on
	// how long the machine took to run the lookups. An atomic offset, because
	// a query the silent server never answered is still reading the clock
	// when the test moves it.
	base := time.Now()
	var offset atomic.Int64
	r.nowFunc = func() time.Time { return base.Add(time.Duration(offset.Load())) }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, _, err := r.LookupFamilies(ctx, "carrier.test"); err != nil {
		t.Fatalf("first LookupFamilies() = %v", err)
	}
	asked := silent.Queries()
	if _, _, err := r.LookupFamilies(ctx, "carrier.test"); err != nil {
		t.Fatalf("second LookupFamilies() = %v", err)
	}
	if n := silent.Queries(); n != asked {
		t.Fatalf("the dark servers were asked %d times within the window", n-asked)
	}

	offset.Store(int64(ringDarkWindow + time.Second))
	_, v4, err := r.LookupFamilies(ctx, "carrier.test")
	if err != nil {
		t.Fatalf("LookupFamilies() after the window = %v", err)
	}
	if len(v4) != 1 || v4[0].String() != "192.0.2.30" {
		t.Fatalf("LookupFamilies() after the window v4 = %v, want the host's [192.0.2.30]", v4)
	}
	if n := silent.Queries(); n == asked {
		t.Fatal("the configured servers were not asked again after the window")
	}
}
