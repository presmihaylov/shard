//go:build !linux

package netns

import (
	"net"
	"net/netip"
)

// OriginalDestination has no conntrack to ask off Linux.
func OriginalDestination(net.Conn) (netip.AddrPort, error) { return netip.AddrPort{}, ErrNotLinux }
