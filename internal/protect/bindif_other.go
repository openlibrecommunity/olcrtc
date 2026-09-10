// ai-generated: windows outbound-interface pinning via OLCRTC_BIND_IFINDEX.
// SPDX-License-Identifier: WTFPL

//go:build !windows

package protect

import "syscall"

// bindOutgoingInterface is a no-op off Windows. Android keeps our sockets off
// the tunnel through VpnService.protect, and the Linux desktop app runs olcrtc
// with privileges that route around the TUN, so neither needs the socket option.
//
// ai-generated: this function and its doc comment.
func bindOutgoingInterface(_ string, _ syscall.RawConn) error {
	return nil
}
