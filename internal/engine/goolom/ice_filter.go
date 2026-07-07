package goolom

import (
	"net"
	"strings"
)

func keepICEInterface(name string) bool {
	if name == "" {
		return false
	}
	for _, prefix := range []string{"bridge", "vmenet", "awdl", "llw", "ap", "gif", "stf"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return name != "lo0"
}

func keepICEIP(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	return !(ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19))
}
