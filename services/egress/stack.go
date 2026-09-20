package egress

import (
	"net/netip"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netstack"
	"github.com/presmihaylov/shard/services/network"
)

// StackDrop is one frame the daemon's own stack refused, in the record the host chains' drops take, so a read tells the two apart by the reason alone.
func StackDrop(gateway netip.Addr, d netstack.Drop) Record {
	rule := network.RuleStack
	switch {
	case d.Rule != "":
		// A judged flow names the rule that refused it, the way a ring line names the rule of the chain.
		rule = d.Rule
	case d.Destination == gateway:
		rule = network.RuleLocal
	case isPrivate(d.Destination):
		rule = network.RulePrivate
	}

	// A refused frame may name no destination the stack could read, and the log then carries none.
	address := ""
	if d.Destination.IsValid() {
		address = d.Destination.String()
	}

	return Record{
		Time:    d.Time,
		Source:  SourceHost,
		Verdict: string(models.ActionDeny),
		Port:    d.Port,
		Address: address,
		Rule:    rule,
		Reason:  stackReason(rule, d.Protocol),
	}
}

func isPrivate(addr netip.Addr) bool {
	for _, prefix := range network.Private {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

func stackReason(rule, proto string) string {
	switch rule {
	case network.RuleLocal:
		return "the stack dropped a " + proto + " packet to the host's own address"
	case network.RulePrivate:
		return "the stack dropped a " + proto + " packet to a private address"
	case network.RuleDefault:
		return "the stack dropped a " + proto + " packet: no rule of the policy matches"
	case network.RuleStack:
		return "the stack dropped a " + proto + " packet: the stack forwards no such frame off the gateway"
	}

	return "the stack dropped a " + proto + " packet: the first matching rule of the policy denies it"
}

// Drop writes one stack drop into the sandbox that holds the guest address, the way a ring line lands; a guest no record holds is counted and not written.
func (t *Tailer) Drop(guest netip.Addr, record Record) error {
	_, err := t.attribute(drop{Record: record, source: guest.String()})

	return err
}
