package netstack

import (
	"context"
	"fmt"
	"net"
	"net/netip"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// A test plays the guest with a second stack, which needs to dial out, and a default route to do it through.
func (s *Stack) defaultRoute(gateway netip.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.links {
		s.stack.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4(gateway.As4()), NIC: id})
	}
}

func (s *Stack) dialTCP(ctx context.Context, remote netip.AddrPort) (net.Conn, error) {
	conn, err := gonet.DialContextTCP(ctx, s.stack, tcpip.FullAddress{Addr: tcpip.AddrFrom4(remote.Addr().As4()), Port: remote.Port()}, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", remote, err)
	}

	return conn, nil
}

func (s *Stack) dialUDP(remote netip.AddrPort) (net.Conn, error) {
	conn, err := gonet.DialUDP(s.stack, nil, &tcpip.FullAddress{Addr: tcpip.AddrFrom4(remote.Addr().As4()), Port: remote.Port()}, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("dial udp %s: %w", remote, err)
	}

	return conn, nil
}
