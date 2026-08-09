// SPDX-License-Identifier: WTFPL

//go:build !android

package protect

import (
	"fmt"

	"github.com/pion/transport/v4"
	"github.com/wlynxg/anet"
)

// loadInterfaces enumerates network interfaces using the anet library.
// On non-android platforms anet delegates to Go's net package, which uses
// netlink on Linux and sysctl on Darwin/BSD. Neither is restricted by
// SELinux outside of Android untrusted apps.
func loadInterfaces() ([]*transport.Interface, error) {
	ifs, err := anet.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("anet interfaces: %w", err)
	}

	out := make([]*transport.Interface, 0, len(ifs))
	for i := range ifs {
		ifc := transport.NewInterface(ifs[i])
		addrs, err := anet.InterfaceAddrsByInterface(&ifs[i])
		if err != nil {
			return nil, fmt.Errorf("anet addrs %s: %w", ifs[i].Name, err)
		}
		for _, addr := range addrs {
			ifc.AddAddress(addr)
		}
		out = append(out, ifc)
	}
	return out, nil
}
