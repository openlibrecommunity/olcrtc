package goolom

import (
	"net"
	"testing"
)

func TestKeepICEIPRejectsUnusableLocalCandidates(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		{name: "wifi private", ip: "192.168.1.66", want: true},
		{name: "tailscale cgnat", ip: "100.79.221.73", want: true},
		{name: "public", ip: "8.8.8.8", want: true},
		{name: "loopback", ip: "127.0.0.1", want: false},
		{name: "unspecified", ip: "0.0.0.0", want: false},
		{name: "link local", ip: "169.254.85.194", want: false},
		{name: "olc tunnel range", ip: "198.18.0.1", want: false},
		{name: "ipv6", ip: "2001:db8::1", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := keepICEIP(net.ParseIP(tt.ip)); got != tt.want {
				t.Fatalf("keepICEIP(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestKeepICEInterfaceRejectsBridgeOnlyInterfaces(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: "en0", want: true},
		{name: "pdp_ip0", want: true},
		{name: "utun15", want: true},
		{name: "bridge100", want: false},
		{name: "vmenet1", want: false},
		{name: "awdl0", want: false},
		{name: "llw0", want: false},
		{name: "ap1", want: false},
		{name: "lo0", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := keepICEInterface(tt.name); got != tt.want {
				t.Fatalf("keepICEInterface(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
