package serve

import (
	"fmt"
	"math"
	"net"
	"net/netip"
	"sync"
	"syscall"
)

const (
	// preAuthPerSource bounds the connections one source holds with no valid token yet, so one flood cannot spend every fd of the front.
	preAuthPerSource = 32
	// preAuthReserve keeps fds for the listener, the logs and the dials past the held connections.
	preAuthReserve = 64
)

// preAuthTotal caps every connection with no valid token yet at what the fd limit can carry, so the cap never locks out more clients than the limit would (SHARD-372).
func preAuthTotal() (int, error) {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return 0, fmt.Errorf("read the fd limit: %w", err)
	}

	return preAuthTotalFor(limit.Cur), nil
}

// preAuthTotalFor halves the fds past the reserve, since a held connection costs two once it dials the daemon socket.
func preAuthTotalFor(soft uint64) int {
	if soft < preAuthReserve+2*preAuthPerSource {
		return preAuthPerSource
	}

	total := (soft - preAuthReserve) / 2
	if total > math.MaxInt {
		return math.MaxInt
	}

	return int(total)
}

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
