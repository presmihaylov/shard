package portforward

import (
	"fmt"
	"net"

	"github.com/presmihaylov/shard/models"
)

// Reachable lists where a forward answers: the loopback for a private one, and every interface that is up for a public one, less the sandbox bridge.
func (f *Forwarder) Reachable(public bool) ([]models.HostAddress, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list the host interfaces: %w", err)
	}

	out := []models.HostAddress{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Name == f.hidden {
			continue
		}
		if !public && iface.Flags&net.FlagLoopback == 0 {
			continue
		}
		on, err := addresses(iface, public)
		if err != nil {
			return nil, err
		}
		out = append(out, on...)
	}

	return out, nil
}

// addresses lists the IPv4 addresses of one interface a forward answers on.
func addresses(iface net.Interface, public bool) ([]models.HostAddress, error) {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("list the addresses of %s: %w", iface.Name, err)
	}

	var out []models.HostAddress
	for _, addr := range addrs {
		ip, ok := addr.(*net.IPNet)
		if !ok || ip.IP.To4() == nil {
			continue
		}
		// A private forward binds 127.0.0.1 alone, not the rest of 127/8 a loopback may carry.
		if !public && !ip.IP.Equal(net.IPv4(127, 0, 0, 1)) {
			continue
		}
		out = append(out, models.HostAddress{Interface: iface.Name, Address: ip.IP.To4().String()})
	}

	return out, nil
}
