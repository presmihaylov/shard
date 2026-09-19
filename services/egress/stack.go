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
	case d.Destination == gateway:
		rule = network.RuleLocal
	case isPrivate(d.Destination):
		rule = network.RulePrivate
	}

	return Record{
		Time:    d.Time,
		Source:  SourceHost,
		Verdict: string(models.ActionDeny),
		Port:    d.Port,
		Address: d.Destination.String(),
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
	if rule == network.RuleLocal {
		return "the stack dropped a " + proto + " packet to the host's own address"
	}
	if rule == network.RulePrivate {
		return "the stack dropped a " + proto + " packet to a private address"
	}

	return "the stack dropped a " + proto + " packet: a VM reaches nothing off the daemon except through the proxy"
}

// Drop writes one stack drop into the sandbox that holds the guest address, the way a ring line lands; a guest no record holds is counted and not written.
func (t *Tailer) Drop(guest netip.Addr, record Record) error {
	_, err := t.attribute(drop{Record: record, source: guest.String()})

	return err
}
