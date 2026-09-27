package network

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// Local is what the input chain refuses on Linux: the host's own addresses, and what no dial should reach.
type Local struct {
	// Addresses lists the addresses the host owns; nil reads the host's interfaces.
	Addresses func() ([]netip.Addr, error)
	// The host's addresses are read at most once per hostRefresh, so a guest varying its 4-tuples cannot drive the lookup.
	mu    sync.Mutex
	owned map[netip.Addr]struct{}
	err   error
	read  time.Time
}

const hostRefresh = time.Second

var (
	broadcast = netip.MustParseAddr("255.255.255.255")
	// thisNetwork is 0.0.0.0/8, which a dial from the host reads as the host itself.
	thisNetwork = netip.MustParsePrefix("0.0.0.0/8")
)

// Contains says whether addr is local to the host; a lookup that fails counts as local, so the refusal holds.
func (l *Local) Contains(addr netip.Addr) bool {
	if addr.IsUnspecified() || thisNetwork.Contains(addr) || addr.IsMulticast() || addr == broadcast {
		return true
	}
	owned, err := l.hostOwned()
	if err != nil {
		return true
	}
	_, ok := owned[addr]

	return ok
}

// hostOwned is the cached host address set; a lookup that fails is kept for the interval too, so the refusal holds until one succeeds.
func (l *Local) hostOwned() (map[netip.Addr]struct{}, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.read.IsZero() && time.Since(l.read) < hostRefresh {
		return l.owned, l.err
	}
	l.read = time.Now()
	addrs, err := l.hostAddresses()
	if err != nil {
		l.owned, l.err = nil, err

		return nil, err
	}
	l.owned = make(map[netip.Addr]struct{}, len(addrs))
	for _, a := range addrs {
		l.owned[a] = struct{}{}
	}
	l.err = nil

	return l.owned, nil
}

func (l *Local) hostAddresses() ([]netip.Addr, error) {
	if l.Addresses != nil {
		return l.Addresses()
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	owned := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipNet.IP)
		if !ok {
			continue
		}
		owned = append(owned, ip.Unmap())
	}

	return owned, nil
}
