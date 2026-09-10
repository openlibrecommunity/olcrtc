// SPDX-License-Identifier: WTFPL

//go:build windows

package protect

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/windows"
)

// BindInterfaceEnv carries the interface index olcrtc must send its own traffic
// from. The host app knows it: it is the index of the default route BEFORE the
// TUN is raised.
//
// This is the Windows counterpart of Android's VpnService.protect(). Without it
// every socket we open follows the route table, and once the TUN owns the
// default route that includes the calls olcrtc makes to reach a conference in
// the first place - so a client that lost its room could never ask for a new
// one, having just lost the tunnel that question would have travelled through.
const BindInterfaceEnv = "OLCRTC_BIND_IFINDEX"

// Winsock socket options that pin the outgoing interface. Not exported by
// x/sys/windows, so they are declared here.
const (
	ipUnicastIF   = 31 // IPPROTO_IP level
	ipv6UnicastIF = 31 // IPPROTO_IPV6 level
)

//nolint:gochecknoglobals // the bound interface is a process-wide property, like the Android protector
var bindIfIndex atomic.Uint32

//nolint:gochecknoinits // the host app passes the interface in the environment before we open any socket
func init() {
	SetBindInterfaceIndex(parseIfIndex(os.Getenv(BindInterfaceEnv)))
}

func parseIfIndex(raw string) uint32 {
	value, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 32)
	if err != nil {
		return 0
	}
	return uint32(value)
}

// SetBindInterfaceIndex pins every socket we open to this interface index.
// Zero restores the default route-table behaviour.
func SetBindInterfaceIndex(index uint32) {
	bindIfIndex.Store(index)
}

func bindOutgoingInterface(network string, c syscall.RawConn) error {
	index := bindIfIndex.Load()
	if index == 0 {
		return nil
	}
	var optErr error
	controlErr := c.Control(func(fd uintptr) {
		handle := windows.Handle(fd)
		if isIPv6Network(network) {
			// IPV6_UNICAST_IF takes the index in host byte order.
			optErr = windows.SetsockoptInt(handle, windows.IPPROTO_IPV6, ipv6UnicastIF, int(index))
			return
		}
		// IP_UNICAST_IF takes the index in NETWORK byte order, unlike its IPv6
		// sibling. Build the bytes explicitly rather than assuming the host is
		// little-endian.
		var wire [4]byte
		binary.BigEndian.PutUint32(wire[:], index)
		optErr = windows.SetsockoptInt(
			handle, windows.IPPROTO_IP, ipUnicastIF, int(binary.NativeEndian.Uint32(wire[:])),
		)
	})
	if controlErr != nil {
		return fmt.Errorf("bind interface control: %w", controlErr)
	}
	if optErr != nil {
		return &net.OpError{
			Op:  "bind-interface",
			Net: network,
			Err: fmt.Errorf("set unicast interface %d: %w", index, optErr),
		}
	}
	return nil
}

func isIPv6Network(network string) bool {
	return strings.HasSuffix(network, "6")
}
