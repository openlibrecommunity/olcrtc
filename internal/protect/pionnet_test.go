// SPDX-License-Identifier: WTFPL

package protect

import (
	"context"
	"errors"
	"net"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/pion/transport/v4"

	"github.com/openlibrecommunity/olcrtc/internal/fakedns"
)

func TestIsTunInterface(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"tun0":   true,
		"tun":    true,
		"ppp0":   true,
		"pptp0":  true,
		"wlan0":  false,
		"eth0":   false,
		"rmnet0": false,
		"lo":     false,
	}
	for name, want := range cases {
		if got := isTunInterface(name); got != want {
			t.Errorf("isTunInterface(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestInterfacesHidesTun(t *testing.T) {
	t.Parallel()

	n, err := NewProtectedNet()
	if err != nil {
		t.Fatalf("NewProtectedNet: %v", err)
	}
	ifaces, err := n.Interfaces()
	if err != nil {
		t.Fatalf("Interfaces: %v", err)
	}
	for _, ifc := range ifaces {
		if isTunInterface(ifc.Name) {
			t.Errorf("Interfaces returned tun device %q", ifc.Name)
		}
	}
}

func TestInterfaceByNameRejectsTun(t *testing.T) {
	t.Parallel()

	n, err := NewProtectedNet()
	if err != nil {
		t.Fatalf("NewProtectedNet: %v", err)
	}
	if _, err := n.InterfaceByName("tun0"); !errors.Is(err, transport.ErrInterfaceNotFound) {
		t.Errorf("InterfaceByName(tun0) error = %v, want %v", err, transport.ErrInterfaceNotFound)
	}
}

// TestControlFuncFailClosed verifies that the protector can reject a socket.
func TestControlFuncFailClosed(t *testing.T) {
	restoreProtector(t)
	SetProtector(func(int) bool { return false })
	lc := net.ListenConfig{Control: controlFunc}
	pc, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err == nil {
		_ = pc.Close()
		t.Fatal("expected protected ListenPacket to fail when the protector rejects fd")
	}
}

// TestControlFuncProtects verifies that the protector receives a real fd.
func TestControlFuncProtects(t *testing.T) {
	restoreProtector(t)

	var calls int
	SetProtector(func(fd int) bool {
		if fd < 0 {
			t.Errorf("protector got negative fd %d", fd)
		}
		calls++
		return true
	})
	lc := net.ListenConfig{Control: controlFunc}
	pc, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	defer func() { _ = pc.Close() }()
	if calls == 0 {
		t.Error("protector was not invoked")
	}
}

func TestCreateDialerUsesResolver(t *testing.T) {
	resolver := &net.Resolver{PreferGo: true}
	n := &ProtectedNet{resolver: resolver}
	dialer, ok := n.CreateDialer(nil).(resolvingDialer)
	if !ok {
		t.Fatalf("CreateDialer() type = %T, want resolvingDialer", n.CreateDialer(nil))
	}
	if dialer.lookup != resolver {
		t.Fatalf("CreateDialer().lookup = %v, want %p", dialer.lookup, resolver)
	}
	if _, ok := dialer.dialer.(*protectedDialer); !ok {
		t.Fatalf("resolvingDialer wraps %T, want *protectedDialer", dialer.dialer)
	}
	plain := &ProtectedNet{}
	if _, ok := plain.CreateDialer(nil).(*protectedDialer); !ok {
		t.Fatalf("CreateDialer() without a lookup = %T, want *protectedDialer", plain.CreateDialer(nil))
	}
}

func TestResolveAddrUsesInjectedResolverSemantics(t *testing.T) {
	t.Parallel()

	n := &ProtectedNet{resolver: &net.Resolver{PreferGo: true}}

	tcp, err := n.ResolveTCPAddr("tcp", "127.0.0.1:http")
	if err != nil {
		t.Fatalf("ResolveTCPAddr service: %v", err)
	}
	if !tcp.IP.Equal(net.IPv4(127, 0, 0, 1)) || tcp.Port != 80 {
		t.Errorf("ResolveTCPAddr service = %v, want 127.0.0.1:80", tcp)
	}

	udp, err := n.ResolveUDPAddr("udp6", "[fe80::1%test-zone]:domain")
	if err != nil {
		t.Fatalf("ResolveUDPAddr zone: %v", err)
	}
	if !udp.IP.Equal(net.ParseIP("fe80::1")) || udp.Port != 53 || udp.Zone != "test-zone" {
		t.Errorf("ResolveUDPAddr zone = %#v, want [fe80::1%%test-zone]:53", udp)
	}

	ip, err := n.ResolveIPAddr(ipNetwork6, "fe80::2%test-zone")
	if err != nil {
		t.Fatalf("ResolveIPAddr zone: %v", err)
	}
	if !ip.IP.Equal(net.ParseIP("fe80::2")) || ip.Zone != "test-zone" {
		t.Errorf("ResolveIPAddr zone = %#v, want fe80::2%%test-zone", ip)
	}

	empty, err := n.ResolveTCPAddr("", "")
	if err != nil {
		t.Fatalf("ResolveTCPAddr empty address: %v", err)
	}
	if empty.IP != nil || empty.Port != 0 || empty.Zone != "" {
		t.Errorf("ResolveTCPAddr empty address = %#v, want zero TCPAddr", empty)
	}

	emptyHost, err := n.ResolveUDPAddr("udp", ":53")
	if err != nil {
		t.Fatalf("ResolveUDPAddr empty host: %v", err)
	}
	if emptyHost.IP != nil || emptyHost.Port != 53 || emptyHost.Zone != "" {
		t.Errorf("ResolveUDPAddr empty host = %#v, want :53", emptyHost)
	}
}

func TestResolveAddrRejectsInvalidPortAndFamily(t *testing.T) {
	t.Parallel()

	n := &ProtectedNet{resolver: &net.Resolver{PreferGo: true}}
	for _, address := range []string{"127.0.0.1:-1", "127.0.0.1:65536"} {
		if _, err := n.ResolveTCPAddr("tcp", address); err == nil {
			t.Errorf("ResolveTCPAddr(%q) unexpectedly succeeded", address)
		}
	}
	if _, err := n.ResolveTCPAddr("tcp4", "[::1]:80"); err == nil {
		t.Error("ResolveTCPAddr tcp4 IPv6 literal unexpectedly succeeded")
	}
	if _, err := n.ResolveUDPAddr("udp6", "127.0.0.1:53"); err == nil {
		t.Error("ResolveUDPAddr udp6 IPv4 literal unexpectedly succeeded")
	}
	if _, err := n.ResolveIPAddr(ipNetwork4, "::1"); err == nil {
		t.Error("ResolveIPAddr ip4 IPv6 literal unexpectedly succeeded")
	}
}

func TestLookupIPAddrSelection(t *testing.T) {
	t.Parallel()

	ips := []net.IPAddr{
		{IP: net.ParseIP("2001:db8::1")},
		{IP: net.IPv4(192, 0, 2, 1)},
	}
	for _, tc := range []struct {
		network string
		want    net.IP
	}{
		{network: "ip", want: net.IPv4(192, 0, 2, 1)},
		{network: ipNetwork4, want: net.IPv4(192, 0, 2, 1)},
		{network: ipNetwork6, want: net.ParseIP("2001:db8::1")},
	} {
		got, err := selectIPAddr(ips, tc.network, "example.test")
		if err != nil {
			t.Fatalf("selectIPAddr(%q): %v", tc.network, err)
		}
		if !got.IP.Equal(tc.want) {
			t.Errorf("selectIPAddr(%q) = %v, want %v", tc.network, got.IP, tc.want)
		}
	}
}

// TestCreateDialerProtectsAndChains verifies that CreateDialer copies the
// caller's Dialer and keeps the caller's Control hook.
func TestCreateDialerProtectsAndChains(t *testing.T) {
	restoreProtector(t)

	var protectorRan bool
	SetProtector(func(int) bool { protectorRan = true; return true })

	n, err := NewProtectedNet()
	if err != nil {
		t.Fatalf("NewProtectedNet: %v", err)
	}

	// Dial a local TCP listener.
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		if c, aerr := ln.Accept(); aerr == nil {
			_ = c.Close()
		}
	}()

	var callerControlRan bool
	caller := &net.Dialer{
		Control: func(_, _ string, _ syscall.RawConn) error {
			callerControlRan = true
			return nil
		},
	}
	callerControl := caller.Control

	dialer := n.CreateDialer(caller)

	// Keep the caller's Control unchanged.
	if reflect.ValueOf(caller.Control).Pointer() != reflect.ValueOf(callerControl).Pointer() {
		t.Error("CreateDialer mutated the caller's Dialer.Control")
	}

	conn, err := dialer.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial via CreateDialer: %v", err)
	}
	_ = conn.Close()

	if !protectorRan {
		t.Error("protector hook did not run for the CreateDialer dialer")
	}
	if !callerControlRan {
		t.Error("caller's Control hook did not run (chain dropped it)")
	}
}

// TestCreateDialerProtectsAndChainsControlContext verifies that CreateDialer
// keeps the caller's ControlContext hook.
func TestCreateDialerProtectsAndChainsControlContext(t *testing.T) {
	restoreProtector(t)

	var protectorRan bool
	SetProtector(func(int) bool { protectorRan = true; return true })

	n, err := NewProtectedNet()
	if err != nil {
		t.Fatalf("NewProtectedNet: %v", err)
	}

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		if c, aerr := ln.Accept(); aerr == nil {
			_ = c.Close()
		}
	}()

	var callerControlContextRan bool
	caller := &net.Dialer{
		ControlContext: func(_ context.Context, _, _ string, _ syscall.RawConn) error {
			callerControlContextRan = true
			return nil
		},
	}
	callerControlContext := caller.ControlContext

	dialer := n.CreateDialer(caller)

	if reflect.ValueOf(caller.ControlContext).Pointer() != reflect.ValueOf(callerControlContext).Pointer() {
		t.Error("CreateDialer mutated the caller's Dialer.ControlContext")
	}

	conn, err := dialer.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial via CreateDialer: %v", err)
	}
	_ = conn.Close()

	if !protectorRan {
		t.Error("protector hook did not run for the CreateDialer dialer")
	}
	if !callerControlContextRan {
		t.Error("caller's ControlContext hook did not run (chain dropped it)")
	}
}

const turnTestIP = "192.0.2.40"

// Pion resolves its STUN and TURN servers through the Net it is handed, and
// the shim used to leave that to Pion's standard net - the system resolver,
// which inside an iOS packet tunnel is the tunnel's own, unserved until the
// cores are up. Server names now go the way of every other protected lookup:
// the configured servers first, the host's resolver behind them.
func TestProtectedNetResolvesServerNamesThroughTheConfiguredServers(t *testing.T) {
	dns, err := fakedns.Start(map[string]string{"turn.test": turnTestIP})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dns.Close() }()

	n, err := NewProtectedNet(NewResolver(dns.Addr))
	if err != nil {
		t.Fatal(err)
	}
	udp, err := n.ResolveUDPAddr("udp4", "turn.test:3478")
	if err != nil {
		t.Fatalf("ResolveUDPAddr() = %v", err)
	}
	if udp.IP.String() != turnTestIP || udp.Port != 3478 {
		t.Fatalf("ResolveUDPAddr() = %v, want %s:3478", udp, turnTestIP)
	}
	tcp, err := n.ResolveTCPAddr("tcp", "turn.test:443")
	if err != nil {
		t.Fatalf("ResolveTCPAddr() = %v", err)
	}
	if tcp.IP.String() != turnTestIP || tcp.Port != 443 {
		t.Fatalf("ResolveTCPAddr() = %v, want %s:443", tcp, turnTestIP)
	}
	if dns.Queries() == 0 {
		t.Fatal("the configured server was never asked")
	}
}

// A literal passes through untouched, zone and all: link-local candidates
// carry one, and a lookup would have nothing to add.
func TestProtectedNetKeepsALiteralAddressAsItIs(t *testing.T) {
	n, err := NewProtectedNet(NewResolver("127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	literal, err := n.ResolveUDPAddr("udp", "[fe80::1%lo]:1")
	if err != nil {
		t.Fatalf("ResolveUDPAddr() of a literal = %v", err)
	}
	if literal.Zone != "lo" || literal.Port != 1 || literal.IP.String() != "fe80::1" {
		t.Fatalf("ResolveUDPAddr() of a literal = %v, want fe80::1 with zone lo and port 1", literal)
	}
}

// And when the configured servers are dark, the host's resolver answers for
// Pion too - the same fallback the dialers have, not a lookup that dies with
// the first silent server.
func TestProtectedNetFallsBackToTheHostResolver(t *testing.T) {
	silent, err := fakedns.StartSilent()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = silent.Close() }()
	host, err := fakedns.Start(map[string]string{"turn.test": "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = host.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pc, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	_, port, _ := net.SplitHostPort(pc.LocalAddr().String())

	r := newTestResolver(t, silent.Addr, host)
	n, err := NewProtectedNet(r)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := n.Dial("udp", net.JoinHostPort("turn.test", port))
	if err != nil {
		t.Fatalf("Dial() = %v, want the host's resolver to answer", err)
	}
	_ = conn.Close()
	direct, err := NewDialer(r).DialContext(ctx, "udp", net.JoinHostPort("turn.test", port))
	if err != nil {
		t.Fatalf("DialContext(udp) = %v, want the host's resolver to answer", err)
	}
	_ = direct.Close()
}

// The pion dialers used to be plain net.Dialers with a Control hook and nothing
// else, so their name lookups went to the system resolver. Inside an iOS packet
// tunnel that resolver is the tunnel's own, which nothing is serving yet when
// the carrier is being dialed - a self-hosted Jitsi host, never in the phone's
// DNS cache, failed with "lookup meet.example: no such host" (olcbox#13).
func TestProtectedNetDialResolvesThroughTheConfiguredServer(t *testing.T) {
	carrier, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = carrier.Close() }()
	go func() {
		for {
			c, acceptErr := carrier.Accept()
			if acceptErr != nil {
				return
			}
			_ = c.Close()
		}
	}()

	dns, err := fakedns.Start(map[string]string{"carrier.test": "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dns.Close() }()

	pnet, err := NewProtectedNet(NewResolver(dns.Addr))
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(carrier.Addr().String())
	conn, err := pnet.Dial("tcp", net.JoinHostPort("carrier.test", port))
	if err != nil {
		t.Fatalf("Dial() = %v, want a connection resolved through %s", err, dns.Addr)
	}
	_ = conn.Close()
	if dns.Queries() == 0 {
		t.Fatal("the configured server was never asked")
	}
}
