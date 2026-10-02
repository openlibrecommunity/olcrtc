package mobile

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/fakedns"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
	"github.com/openlibrecommunity/olcrtc/pkg/olcrtc/client"
)

// listenCarrier stands in for a carrier's host: accepts and hangs up.
func listenCarrier(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
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
	return port
}

// dialingRunner dials the carrier by name through the resolver the client
// config carries, the way the engines do, and reports how that went.
func dialingRunner(port string, dialed chan<- error) clientRunner {
	return func(ctx context.Context, cfg client.Config, onReady func(string)) error {
		conn, dialErr := protect.NewDialer(cfg.Resolver).DialContext(ctx, "tcp", net.JoinHostPort("carrier.test", port))
		if dialErr == nil {
			_ = conn.Close()
		}
		dialed <- dialErr
		onReady(cfg.LocalAddr)
		<-ctx.Done()
		return ctx.Err()
	}
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

// The client is handed the runtime's own resolver, and that resolver asks
// the servers SetDNS named (olcbox#13).
func TestStartHandsTheRuntimeResolverToTheClient(t *testing.T) {
	port := listenCarrier(t)
	dns := startDNS(t, map[string]string{"carrier.test": "127.0.0.1"})

	dialed := make(chan error, 1)
	runtime := configuredRuntime(t, dialingRunner(port, dialed))
	if err := runtime.SetDNS(dns.Addr); err != nil {
		t.Fatalf("SetDNS() error = %v", err)
	}
	if err := runtime.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = runtime.Stop(1000) }()
	select {
	case dialErr := <-dialed:
		if dialErr != nil {
			t.Fatalf("carrier dial = %v, want it resolved through %s", dialErr, dns.Addr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the client never dialed")
	}
}

// The list is an order: the silent first server is asked, the second answers.
func TestSetDNSTakesAListAndAsksItInOrder(t *testing.T) {
	port := listenCarrier(t)
	silent, err := fakedns.StartSilent()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	answering := startDNS(t, map[string]string{"carrier.test": "127.0.0.1"})

	dialed := make(chan error, 1)
	runtime := configuredRuntime(t, dialingRunner(port, dialed))
	if err := runtime.SetDNS(silent.Addr + ", " + answering.Addr); err != nil {
		t.Fatalf("SetDNS() error = %v", err)
	}
	if err := runtime.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = runtime.Stop(1000) }()
	select {
	case dialErr := <-dialed:
		if dialErr != nil {
			t.Fatalf("carrier dial = %v, want the second server's answer", dialErr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the client never dialed")
	}
	if silent.Queries() == 0 || answering.Queries() == 0 {
		t.Fatalf("queries silent=%d answering=%d, want both asked", silent.Queries(), answering.Queries())
	}
}

// SetDNS while a generation runs reaches that generation: the platform
// calls it when the network under the tunnel changes.
func TestSetDNSAppliesToARunningGeneration(t *testing.T) {
	resolvers := make(chan *protect.Resolver, 1)
	runtime := configuredRuntime(t, func(ctx context.Context, cfg client.Config, onReady func(string)) error {
		r, _ := cfg.Resolver.(*protect.Resolver)
		resolvers <- r
		onReady(cfg.LocalAddr)
		<-ctx.Done()
		return ctx.Err()
	})
	if err := runtime.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = runtime.Stop(1000) }()
	r := <-resolvers
	if r == nil {
		t.Fatal("the client did not get the runtime's *protect.Resolver")
	}
	if err := runtime.SetDNS("10.9.9.9:53"); err != nil {
		t.Fatalf("SetDNS() error = %v", err)
	}
	if got := r.Servers(); len(got) == 0 || got[0] != "10.9.9.9:53" {
		t.Fatalf("running generation's servers = %v, want 10.9.9.9:53 first", got)
	}
}
