//go:build linux

package netns

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"golang.org/x/sys/unix"
)

// OriginalDestination is where the peer sent a connection before a dnat rule turned it to this listener, which conntrack keeps.
func OriginalDestination(conn net.Conn) (netip.AddrPort, error) {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("a %T carries no original destination", conn)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}

	var mreq *unix.IPv6Mreq
	var optErr error
	// SO_ORIGINAL_DST answers a sockaddr_in; IPv6Mreq is the getter whose buffer holds one.
	if err := raw.Control(func(fd uintptr) {
		mreq, optErr = unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST)
	}); err != nil {
		return netip.AddrPort{}, err
	}
	if optErr != nil {
		return netip.AddrPort{}, fmt.Errorf("read the original destination: %w", optErr)
	}

	sa := mreq.Multiaddr
	addr := netip.AddrFrom4([4]byte(sa[4:8]))

	return netip.AddrPortFrom(addr, binary.BigEndian.Uint16(sa[2:4])), nil
}
