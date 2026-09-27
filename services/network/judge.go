package network

import (
	"net/netip"
	"slices"
	"sync"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netstack"
)

// Judge answers for a stack what the egress chain answers on a Linux host: the private floor, then the sandbox's rules in order, then the default drop.
type Judge struct {
	Local   Local
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
	if j.Local.Contains(f.Destination.Addr()) {
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

// Fronted says the guest's 80 and 443 go through the proxy: the last apply compiled a chain for it, as the host dnats only for a chain.
func (j *Judge) Fronted(guest netip.Addr) bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	_, ok := j.chains[guest]

	return ok
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
