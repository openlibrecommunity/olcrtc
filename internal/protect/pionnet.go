// SPDX-License-Identifier: WTFPL

// ProtectedNet wraps Pion's network adapter. It applies the configured protector to each
// socket fd and hides tunnel-style interfaces from candidate gathering.
//
// On Android 11+ (API 30) SELinux denies untrusted_app from binding
// netlink_route_socket (b/155595000). Go's net.Interfaces() and the anet
// library both use AF_NETLINK internally, so ProtectedNet must not depend
// on stdnet.Net (whose constructor calls anet.Interfaces at init time).
// Instead, interface enumeration is split into platform-specific files:
//
//   - pionnet_default.go: uses anet (net.Interfaces wrapper) on non-android.
//   - pionnet_android.go: uses getifaddrs(3) via cgo, no netlink at all.
//   - pionnet_android_nocgo.go: returns an error (cgo required on android).

package protect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"

	"github.com/pion/transport/v4"
)

// ErrUnexpectedConnType is returned when a protected listen/dial yields an
// unexpected concrete type. The caller closes that connection instead of
// using an unprotected fallback.
var ErrUnexpectedConnType = errors.New("protect: unexpected connection type")

// tunInterfacePrefixes lists interface name prefixes excluded from candidate
// gathering. Keep pptp explicit; it does not match the ppp prefix.
//
//nolint:gochecknoglobals // fixed lookup table; a slice cannot be const
var tunInterfacePrefixes = []string{"tun", "ppp", "pptp"}

const (
	ipNetwork4           = "ip4"
	ipNetwork6           = "ip6"
	errNoSuitableAddress = "no suitable address found"
)

// ProtectedNet implements pion's transport.Net with socket protection and
// tunnel-interface filtering.
type ProtectedNet struct {
	resolver Lookup
}

// NewProtectedNet builds a ProtectedNet with platform-specific interface
// enumeration.
//
// Enumeration is deliberately not probed or cached here: every Interfaces()
// call re-reads the live list because interfaces come and go at runtime on
// mobile (Wi-Fi to cellular handover), and a snapshot taken at construction
// would hand pion a stale set for the rest of the session. Enumeration
// failures therefore surface from Interfaces(), not from here.
//
//nolint:unparam // error kept so the signature stays stable for the engine call sites
func NewProtectedNet(lookups ...Lookup) (*ProtectedNet, error) {
	return &ProtectedNet{resolver: firstLookup(lookups)}, nil
}

// Interfaces returns system interfaces after filtering tunnel-style devices.
func (n *ProtectedNet) Interfaces() ([]*transport.Interface, error) {
	interfaces, err := loadInterfaces()
	if err != nil {
		return nil, fmt.Errorf("load interfaces: %w", err)
	}
	out := make([]*transport.Interface, 0, len(interfaces))
	for _, ifc := range interfaces {
		if !isTunInterface(ifc.Name) {
			out = append(out, ifc)
		}
	}
	return out, nil
}

// InterfaceByIndex returns the interface specified by index.
func (n *ProtectedNet) InterfaceByIndex(index int) (*transport.Interface, error) {
	interfaces, err := loadInterfaces()
	if err != nil {
		return nil, fmt.Errorf("load interfaces: %w", err)
	}
	for _, ifc := range interfaces {
		if ifc.Index == index {
			if isTunInterface(ifc.Name) {
				return nil, transport.ErrInterfaceNotFound
			}
			return ifc, nil
		}
	}
	return nil, fmt.Errorf("%w: index=%d", transport.ErrInterfaceNotFound, index)
}

// InterfaceByName applies the same filtering as Interfaces.
func (n *ProtectedNet) InterfaceByName(name string) (*transport.Interface, error) {
	if isTunInterface(name) {
		return nil, transport.ErrInterfaceNotFound
	}
	interfaces, err := loadInterfaces()
	if err != nil {
		return nil, fmt.Errorf("load interfaces: %w", err)
	}
	for _, ifc := range interfaces {
		if ifc.Name == name {
			return ifc, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", transport.ErrInterfaceNotFound, name)
}

func isTunInterface(name string) bool {
	for _, p := range tunInterfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ListenPacket listens for packets on a protected socket.
func (n *ProtectedNet) ListenPacket(network, address string) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: controlFunc}
	conn, err := lc.ListenPacket(context.Background(), network, address)
	if err != nil {
		return nil, fmt.Errorf("listen packet %s %q: %w", network, address, err)
	}
	return conn, nil
}

// ListenUDP listens for UDP packets on a protected socket.
func (n *ProtectedNet) ListenUDP(network string, locAddr *net.UDPAddr) (transport.UDPConn, error) {
	lc := net.ListenConfig{Control: controlFunc}
	address := udpAddrString(locAddr)
	pc, err := lc.ListenPacket(context.Background(), network, address)
	if err != nil {
		return nil, fmt.Errorf("listen udp %s %q: %w", network, address, err)
	}
	uc, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return nil, ErrUnexpectedConnType
	}
	return uc, nil
}

// Dial connects to the address on a protected socket. The name in it goes
// the way of every other name here: through the session's lookup, so a TURN
// or signalling host is never left to the system resolver, which inside an
// iOS packet tunnel is the tunnel's own (olcbox#16).
func (n *ProtectedNet) Dial(network, address string) (net.Conn, error) {
	conn, err := NewDialer(n.resolver).DialContext(context.Background(), network, address)
	if err != nil {
		return nil, fmt.Errorf("dial %s %q: %w", network, address, err)
	}
	return conn, nil
}

// DialUDP connects to a UDP address on a protected socket.
func (n *ProtectedNet) DialUDP(network string, laddr, raddr *net.UDPAddr) (transport.UDPConn, error) {
	d := net.Dialer{Control: controlFunc}
	if laddr != nil {
		d.LocalAddr = laddr
	}
	address := udpAddrString(raddr)
	conn, err := d.Dial(network, address)
	if err != nil {
		return nil, fmt.Errorf("dial udp %s %q: %w", network, address, err)
	}
	uc, ok := conn.(*net.UDPConn)
	if !ok {
		_ = conn.Close()
		return nil, ErrUnexpectedConnType
	}
	return uc, nil
}

// DialTCP connects to a TCP address on a protected socket.
func (n *ProtectedNet) DialTCP(network string, laddr, raddr *net.TCPAddr) (transport.TCPConn, error) {
	d := net.Dialer{Control: controlFunc}
	if laddr != nil {
		d.LocalAddr = laddr
	}
	address := tcpAddrString(raddr)
	conn, err := d.Dial(network, address)
	if err != nil {
		return nil, fmt.Errorf("dial tcp %s %q: %w", network, address, err)
	}
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		_ = conn.Close()
		return nil, ErrUnexpectedConnType
	}
	return tc, nil
}

// ListenTCP listens for TCP connections on a protected socket.
func (n *ProtectedNet) ListenTCP(network string, laddr *net.TCPAddr) (transport.TCPListener, error) {
	lc := net.ListenConfig{Control: controlFunc}
	address := tcpAddrString(laddr)
	l, err := lc.Listen(context.Background(), network, address)
	if err != nil {
		return nil, fmt.Errorf("listen tcp %s %q: %w", network, address, err)
	}
	tl, ok := l.(*net.TCPListener)
	if !ok {
		_ = l.Close()
		return nil, ErrUnexpectedConnType
	}
	return protectedTCPListener{tl}, nil
}

// ResolveIPAddr returns an address of IP end point.
func (n *ProtectedNet) ResolveIPAddr(network, address string) (*net.IPAddr, error) {
	if n.resolver == nil {
		addr, err := net.ResolveIPAddr(network, address)
		if err != nil {
			return nil, fmt.Errorf("resolve ip %s %q: %w", network, address, err)
		}
		return addr, nil
	}
	ipNetwork, err := resolveIPNetwork(network)
	if err != nil {
		return nil, fmt.Errorf("resolve ip %s %q: %w", network, address, err)
	}
	ip, err := lookupIPAddr(n.resolver, ipNetwork, address)
	if err != nil {
		return nil, fmt.Errorf("resolve ip %s %q: %w", network, address, err)
	}
	return &ip, nil
}

// ResolveUDPAddr returns an address of UDP end point.
func (n *ProtectedNet) ResolveUDPAddr(network, address string) (*net.UDPAddr, error) {
	if n.resolver == nil {
		addr, err := net.ResolveUDPAddr(network, address)
		if err != nil {
			return nil, fmt.Errorf("resolve udp %s %q: %w", network, address, err)
		}
		return addr, nil
	}
	resolvedNetwork, err := resolveTransportNetwork(network, "udp")
	if err != nil {
		return nil, fmt.Errorf("resolve udp %s %q: %w", network, address, err)
	}
	network = resolvedNetwork
	ip, port, err := n.resolveHostPort(network, address)
	if err != nil {
		return nil, fmt.Errorf("resolve udp %s %q: %w", network, address, err)
	}
	return &net.UDPAddr{IP: ip.IP, Port: port, Zone: ip.Zone}, nil
}

// ResolveTCPAddr returns an address of TCP end point.
func (n *ProtectedNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	if n.resolver == nil {
		addr, err := net.ResolveTCPAddr(network, address)
		if err != nil {
			return nil, fmt.Errorf("resolve tcp %s %q: %w", network, address, err)
		}
		return addr, nil
	}
	resolvedNetwork, err := resolveTransportNetwork(network, "tcp")
	if err != nil {
		return nil, fmt.Errorf("resolve tcp %s %q: %w", network, address, err)
	}
	network = resolvedNetwork
	ip, port, err := n.resolveHostPort(network, address)
	if err != nil {
		return nil, fmt.Errorf("resolve tcp %s %q: %w", network, address, err)
	}
	return &net.TCPAddr{IP: ip.IP, Port: port, Zone: ip.Zone}, nil
}

func resolveTransportNetwork(network, protocol string) (string, error) {
	if network == "" {
		return protocol, nil
	}
	if network == protocol || network == protocol+"4" || network == protocol+"6" {
		return network, nil
	}
	return "", net.UnknownNetworkError(network)
}

func resolveIPNetwork(network string) (string, error) {
	if network == "" {
		return "ip", nil
	}
	if network == "ip" || network == ipNetwork4 || network == ipNetwork6 {
		return network, nil
	}
	if i := strings.LastIndexByte(network, ':'); i >= 0 {
		// Let net validate protocol names and numbers without resolving a host.
		if _, err := net.ResolveIPAddr(network, ""); err != nil {
			return "", fmt.Errorf("validate IP network: %w", err)
		}
		return network[:i], nil
	}
	return "", net.UnknownNetworkError(network)
}

func (n *ProtectedNet) resolveHostPort(network, address string) (net.IPAddr, int, error) {
	if address == "" {
		return net.IPAddr{}, 0, nil
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return net.IPAddr{}, 0, fmt.Errorf("split host port: %w", err)
	}
	port, err := net.DefaultResolver.LookupPort(context.Background(), network, portText)
	if err != nil {
		return net.IPAddr{}, 0, fmt.Errorf("lookup port: %w", err)
	}
	ipNetwork := "ip"
	if strings.HasSuffix(network, "4") {
		ipNetwork = ipNetwork4
	} else if strings.HasSuffix(network, "6") {
		ipNetwork = ipNetwork6
	}
	ip, err := lookupIPAddr(n.resolver, ipNetwork, host)
	if err != nil {
		return net.IPAddr{}, 0, err
	}
	return ip, port, nil
}

func lookupIPAddr(resolver Lookup, network, host string) (net.IPAddr, error) {
	if host == "" {
		return net.IPAddr{}, nil
	}
	address, zone := host, ""
	if i := strings.LastIndexByte(host, '%'); i >= 0 {
		address, zone = host[:i], host[i+1:]
	}
	if ip := net.ParseIP(address); ip != nil {
		return selectIPAddr([]net.IPAddr{{IP: ip, Zone: zone}}, network, host)
	}
	ips, err := resolver.LookupIP(context.Background(), network, host)
	if err != nil {
		return net.IPAddr{}, fmt.Errorf("lookup IP: %w", err)
	}
	addrs := make([]net.IPAddr, len(ips))
	for i, ip := range ips {
		addrs[i].IP = ip
	}
	return selectIPAddr(addrs, network, host)
}

func selectIPAddr(ips []net.IPAddr, network, host string) (net.IPAddr, error) {
	if strings.HasSuffix(network, "4") {
		if ip, ok := firstIPAddr(ips, true); ok {
			return ip, nil
		}
		return net.IPAddr{}, &net.AddrError{Err: errNoSuitableAddress, Addr: host}
	}
	if strings.HasSuffix(network, "6") {
		if ip, ok := firstIPAddr(ips, false); ok {
			return ip, nil
		}
		return net.IPAddr{}, &net.AddrError{Err: errNoSuitableAddress, Addr: host}
	}
	// Match net.Resolve*Addr's preference for IPv4 host-name results.
	if ip, ok := firstIPAddr(ips, true); ok {
		return ip, nil
	}
	if ip, ok := firstIPAddr(ips, false); ok {
		return ip, nil
	}
	return net.IPAddr{}, &net.AddrError{Err: errNoSuitableAddress, Addr: host}
}

func firstIPAddr(ips []net.IPAddr, ipv4 bool) (net.IPAddr, bool) {
	for _, ip := range ips {
		is4 := ip.IP.To4() != nil
		if (ipv4 && is4) || (!ipv4 && !is4 && len(ip.IP) == net.IPv6len) {
			return ip, true
		}
	}
	return net.IPAddr{}, false
}

// CreateDialer returns a dialer that protects each fd. It copies d and chains
// any existing Control hook.
func (n *ProtectedNet) CreateDialer(d *net.Dialer) transport.Dialer {
	var dialer net.Dialer
	if d != nil {
		dialer = *d
	}
	if dialer.ControlContext != nil {
		dialer.ControlContext = chainControlContext(dialer.ControlContext)
	} else {
		dialer.Control = chainControl(dialer.Control)
	}
	protected := &protectedDialer{dialer: dialer}
	// Names resolve where the sockets are protected, not through the system
	// resolver - see Dial. A resolver the caller chose is left alone.
	if n.resolver != nil && dialer.Resolver == nil {
		return resolvingDialer{lookup: n.resolver, dialer: protected}
	}
	return protected
}

// resolvingDialer resolves a name before the dialer underneath sees it, so
// pion's TURN and STUN hosts go through the session's lookup.
type resolvingDialer struct {
	lookup Lookup
	dialer transport.Dialer
}

func (d resolvingDialer) Dial(network, address string) (net.Conn, error) {
	literal, err := resolveAddress(context.Background(), d.lookup, network, address)
	if err != nil {
		return nil, fmt.Errorf("dial %s %q: %w", network, address, err)
	}
	conn, err := d.dialer.Dial(network, literal)
	if err != nil {
		return nil, fmt.Errorf("dial %s %q: %w", network, address, err)
	}
	return conn, nil
}

// CreateListenConfig returns a listen config that protects each fd. It copies
// lc and chains any existing Control hook.
func (n *ProtectedNet) CreateListenConfig(lc *net.ListenConfig) transport.ListenConfig {
	var cfg net.ListenConfig
	if lc != nil {
		cfg = *lc
	}
	cfg.Control = chainControl(cfg.Control)
	return &protectedListenConfig{lc: cfg}
}

type protectedDialer struct {
	dialer net.Dialer
}

func (d *protectedDialer) Dial(network, address string) (net.Conn, error) {
	conn, err := d.dialer.Dial(network, address)
	if err != nil {
		return nil, fmt.Errorf("protected dial %s %q: %w", network, address, err)
	}
	return conn, nil
}

type protectedListenConfig struct {
	lc net.ListenConfig
}

func (p *protectedListenConfig) Listen(ctx context.Context, network, address string) (net.Listener, error) {
	l, err := p.lc.Listen(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("protected listen %s %q: %w", network, address, err)
	}
	return l, nil
}

func (p *protectedListenConfig) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	pc, err := p.lc.ListenPacket(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("protected listen packet %s %q: %w", network, address, err)
	}
	return pc, nil
}

// chainControl runs the protector first, then any existing Control hook.
func chainControl(
	next func(network, address string, c syscall.RawConn) error,
) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		if err := controlFunc(network, address, c); err != nil {
			return err
		}
		if next != nil {
			return next(network, address, c)
		}
		return nil
	}
}

// chainControlContext runs the protector first, then any existing ControlContext hook.
func chainControlContext(
	next func(context.Context, string, string, syscall.RawConn) error,
) func(context.Context, string, string, syscall.RawConn) error {
	return func(ctx context.Context, network, address string, c syscall.RawConn) error {
		if err := controlFunc(network, address, c); err != nil {
			return err
		}
		if next != nil {
			return next(ctx, network, address, c)
		}
		return nil
	}
}

type protectedTCPListener struct {
	*net.TCPListener
}

// AcceptTCP accepts the next TCP connection on the protected listener.
func (l protectedTCPListener) AcceptTCP() (transport.TCPConn, error) {
	conn, err := l.TCPListener.AcceptTCP()
	if err != nil {
		return nil, fmt.Errorf("accept tcp: %w", err)
	}
	return conn, nil
}

func udpAddrString(a *net.UDPAddr) string {
	if a == nil {
		return ":0"
	}
	return a.String()
}

func tcpAddrString(a *net.TCPAddr) string {
	if a == nil {
		return ":0"
	}
	return a.String()
}

// Compile-time assertion that ProtectedNet satisfies Pion's Net.
var _ transport.Net = (*ProtectedNet)(nil)
