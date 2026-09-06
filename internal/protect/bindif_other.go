// SPDX-License-Identifier: WTFPL

//go:build !windows

package protect

import "syscall"

// bindOutgoingInterface is a no-op off Windows. Android keeps our sockets off
// the tunnel through VpnService.protect, and the Linux desktop app runs olcrtc
// with privileges that route around the TUN, so neither needs the socket option.
func bindOutgoingInterface(_ string, _ syscall.RawConn) error {
	return nil
}
