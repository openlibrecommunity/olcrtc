package protect

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
)

// staticLookup answers every host with fixed addresses.
type staticLookup struct{ v6, v4 []net.IP }

func (l staticLookup) LookupIP(_ context.Context, network, _ string) ([]net.IP, error) {
	return familiesFor(network, l.v6, l.v4), nil
}

func listenLoopback(t *testing.T, network, host string) (string, func()) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), network, net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("%s on %s unavailable: %v", network, host, err)
	}
	go func() {
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port, func() { _ = ln.Close() }
}

// The v6 leg is refused (nothing listens on ::1 at that port), the v4 leg
// answers: the dial must land on IPv4 instead of reporting the v6 failure.
func TestDialContextFallsToTheOtherFamilyWhenTheFirstIsRefused(t *testing.T) {
	port, stop := listenLoopback(t, "tcp4", "127.0.0.1")
	defer stop()
	d := NewDialer(staticLookup{v6: []net.IP{net.ParseIP("::1")}, v4: []net.IP{net.ParseIP("127.0.0.1")}})
	conn, err := d.DialContext(context.Background(), "tcp", "carrier.test:"+port)
	if err != nil {
		t.Fatalf("DialContext = %v, want the IPv4 leg to connect", err)
	}
	_ = conn.Close()
}

// Both legs fail: the error names both, so a v6-only failure is never
// hidden behind an unrelated v4 message.
func TestDialErrorNamesBothFamilies(t *testing.T) {
	port, stop := listenLoopback(t, "tcp4", "127.0.0.1")
	stop()
	d := NewDialer(staticLookup{v6: []net.IP{net.ParseIP("::1")}, v4: []net.IP{net.ParseIP("127.0.0.1")}})
	_, err := d.DialContext(context.Background(), "tcp", "carrier.test:"+port)
	if err == nil {
		t.Fatal("DialContext succeeded against closed ports")
	}
	var fe *familyDialError
	if !errors.As(err, &fe) || !strings.Contains(err.Error(), "ipv6:") || !strings.Contains(err.Error(), "ipv4:") {
		t.Fatalf("error = %v, want a familyDialError naming ipv6 and ipv4", err)
	}
}

func TestNoRouteIsRetriable(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EHOSTUNREACH, syscall.ENETUNREACH} {
		if !isRetriableError(&net.OpError{Op: "dial", Err: &net.OpError{Op: "connect", Err: errno}}) {
			t.Fatalf("%v is not retriable", errno)
		}
	}
}

func TestProtectorRefusalIsNamed(t *testing.T) {
	SetProtector(func(int) bool { return false })
	defer SetProtector(nil)
	_, err := NewDialer().DialContext(context.Background(), "tcp4", "127.0.0.1:1")
	if !errors.Is(err, ErrProtectorRejected) {
		t.Fatalf("error = %v, want ErrProtectorRejected", err)
	}
}
