package dnsutil

import (
	"context"
	"net"
	"reflect"
	"testing"
	"time"
)

func TestSplitServersTrimsAndDropsEmptyEntries(t *testing.T) {
	got := SplitServers(" 8.8.8.8:53, ,192.168.1.1:53,, ")
	want := []string{"8.8.8.8:53", "192.168.1.1:53"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SplitServers() = %#v, want %#v", got, want)
	}
}

func TestDialServerFallsBackToSecondAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	conn, err := dialServer(context.Background(), "tcp", []string{"127.0.0.1:1", ln.Addr().String()}, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("dialServer() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	select {
	case serverConn := <-accepted:
		_ = serverConn.Close()
	case <-time.After(time.Second):
		t.Fatal("fallback listener did not accept a connection")
	}
}

func TestResolverNetworkPreservesUDPQueries(t *testing.T) {
	if got := resolverNetwork("udp"); got != "udp" {
		t.Fatalf("resolverNetwork(udp) = %q, want udp", got)
	}
	if got := resolverNetwork("udp4"); got != "udp4" {
		t.Fatalf("resolverNetwork(udp4) = %q, want udp4", got)
	}
	if got := resolverNetwork("tcp"); got != "tcp" {
		t.Fatalf("resolverNetwork(tcp) = %q, want tcp", got)
	}
}
