package serve

import (
	"net"
	"net/netip"
	"sync"
)

const (
	// preAuthTotal and preAuthPerSource bound the connections that show no valid token yet, so idle ones cannot spend every fd of the front.
	preAuthTotal     = 256
	preAuthPerSource = 32
)

// preAuth counts the connections that show no valid token yet, in total and per source.
type preAuth struct {
	total     int
	perSource int

	mu      sync.Mutex
	held    int
	sources map[netip.Addr]int
}

func newPreAuth(total, perSource int) *preAuth {
	return &preAuth{total: total, perSource: perSource, sources: map[netip.Addr]int{}}
}

// enter takes a slot for a connection from source, or answers false when either cap is full.
func (p *preAuth) enter(source netip.Addr) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held >= p.total || p.sources[source] >= p.perSource {
		return false
	}
	p.held++
	p.sources[source]++

	return true
}

func (p *preAuth) leave(source netip.Addr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.held--
	p.sources[source]--
	if p.sources[source] == 0 {
		delete(p.sources, source)
	}
}

// sourceOf is the address a connection came from; one the front cannot read shares the bucket of the invalid address.
func sourceOf(remote net.Addr) netip.Addr {
	source, err := netip.ParseAddrPort(remote.String())
	if err != nil {
		return netip.Addr{}
	}

	return source.Addr().Unmap()
}
