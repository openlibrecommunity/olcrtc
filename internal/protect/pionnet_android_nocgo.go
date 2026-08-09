// SPDX-License-Identifier: WTFPL

//go:build android && !cgo

package protect

import (
	"fmt"
	"net"

	"github.com/pion/transport/v4"
)

// loadInterfaces falls back to net.Interfaces() on android without cgo.
// getifaddrs(3) requires cgo; without it there is no netlink-free path.
// On Android 11+ (API 30) this will fail with SELinux denial, but it is
// no worse than the pre-fix behavior.
func loadInterfaces() ([]*transport.Interface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("net interfaces: %w", err)
	}
	out := make([]*transport.Interface, 0, len(ifs))
	for i := range ifs {
		ifc := transport.NewInterface(ifs[i])
		addrs, err := ifs[i].Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ifc.AddAddress(addr)
		}
		out = append(out, ifc)
	}
	return out, nil
}
