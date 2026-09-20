package network

import (
	"net/netip"
	"sync"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netstack"
)

// Judge answers for a stack what the egress chain answers on a Linux host: the private floor, then the sandbox's rules in order, then the default drop.
type Judge struct {
	mu     sync.RWMutex
	chains map[netip.Addr]Chain
}

// Apply replaces every chain at once, the way the host ruleset is replaced in one transaction.
func (j *Judge) Apply(chains []Chain) {
	byAddress := make(map[netip.Addr]Chain, len(chains))
	for _, chain := range chains {
		byAddress[chain.Address] = chain
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.chains = byAddress
}

// Judge rules on one flow by the guest's address; a guest without a policy reaches everything but the floor, like a sandbox on Linux.
func (j *Judge) Judge(f netstack.Flow) netstack.Verdict {
	if contains(Private, f.Destination.Addr()) {
		return netstack.Verdict{Rule: RulePrivate}
	}
	j.mu.RLock()
	chain, ok := j.chains[f.Guest]
	j.mu.RUnlock()
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

// matches is the nft match of render: a prefix set, the protocol when one is named, and the port set when one is.
func matches(rule Compiled, f netstack.Flow) bool {
	if !contains(rule.Prefixes, f.Destination.Addr()) {
		return false
	}
	if rule.Protocol != "" && rule.Protocol != f.Protocol {
		return false
	}
	if len(rule.Ports) == 0 {
		return true
	}
	for _, port := range rule.Ports {
		if port == int(f.Destination.Port()) {
			return true
		}
	}

	return false
}

func contains(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}
