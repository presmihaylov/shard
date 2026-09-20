package network

import (
	"net"
	"net/netip"
	"slices"
	"sync"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netstack"
)

// Judge answers for a stack what the egress chain answers on a Linux host: the private floor, then the sandbox's rules in order, then the default drop.
type Judge struct {
	// Local lists the addresses the host owns, which the Linux input chain refuses; nil reads the host's interfaces.
	Local   func() ([]netip.Addr, error)
	mu      sync.RWMutex
	applied bool
	chains  map[netip.Addr]Chain
}

// Apply replaces every chain at once, the way the host ruleset is replaced in one transaction.
func (j *Judge) Apply(chains []Chain) {
	byAddress := make(map[netip.Addr]Chain, len(chains))
	for _, chain := range chains {
		byAddress[chain.Address] = chain
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.applied = true
	j.chains = byAddress
}

// Judge rules on one flow by the guest's address; a guest without a policy reaches everything but the floor, like a sandbox on Linux.
func (j *Judge) Judge(f netstack.Flow) netstack.Verdict {
	if j.local(f.Destination.Addr()) {
		return netstack.Verdict{Rule: RuleLocal}
	}
	if contains(Private, f.Destination.Addr()) {
		return netstack.Verdict{Rule: RulePrivate}
	}
	j.mu.RLock()
	applied := j.applied
	chain, ok := j.chains[f.Guest]
	j.mu.RUnlock()
	// A stack that adopted running VMs before the first apply refuses until it lands, so a daemon restart opens no window.
	if !applied {
		return netstack.Verdict{Rule: RuleUnapplied}
	}
	if !ok || !chain.Policy {
		return netstack.Verdict{Allow: true, Rule: RuleNone}
	}
	for _, rule := range chain.Rules {
		if matches(rule, f) {
			return netstack.Verdict{Allow: rule.Action == models.ActionAllow, Rule: rule.ID}
		}
	}

	return netstack.Verdict{Rule: RuleDefault}
}

// local is what the input chain refuses on Linux: the host's own addresses, and what no dial should reach; a lookup that fails refuses too.
func (j *Judge) local(addr netip.Addr) bool {
	if addr.IsUnspecified() || addr.IsMulticast() || addr == broadcast {
		return true
	}
	owned, err := j.hostAddresses()
	if err != nil {
		return true
	}

	return slices.Contains(owned, addr)
}

var broadcast = netip.MustParseAddr("255.255.255.255")

func (j *Judge) hostAddresses() ([]netip.Addr, error) {
	if j.Local != nil {
		return j.Local()
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

// matches is the nft match of render: a prefix set, the protocol when one is named, and the port set when one is.
func matches(rule Compiled, f netstack.Flow) bool {
	if !contains(rule.Prefixes, f.Destination.Addr()) {
		return false
	}
	if rule.Protocol != "" && rule.Protocol != f.Protocol {
		return false
	}

	return len(rule.Ports) == 0 || slices.Contains(rule.Ports, int(f.Destination.Port()))
}

func contains(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}
