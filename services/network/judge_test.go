package network

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netstack"
)

var (
	judgedGuest = netip.MustParseAddr("10.200.0.2")
	otherGuest  = netip.MustParseAddr("10.200.0.3")
)

func flow(guest netip.Addr, protocol, destination string) netstack.Flow {
	return netstack.Flow{Guest: guest, Protocol: protocol, Destination: netip.MustParseAddrPort(destination)}
}

// The judge answers as the egress chain does: the floor first, then the rules in order, then the default drop; a guest without a policy reaches everything but the floor.
func TestTheJudgeRulesLikeTheEgressChain(t *testing.T) {
	var j Judge
	j.Apply([]Chain{{Address: judgedGuest, Policy: true, Rules: []Compiled{
		{ID: "no-smtp", Action: models.ActionDeny, Protocol: "tcp", Ports: []int{25}, Prefixes: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}},
		{ID: "mail", Action: models.ActionAllow, Protocol: "tcp", Prefixes: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}},
		{ID: "ntp", Action: models.ActionAllow, Protocol: "udp", Ports: []int{123}, Prefixes: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}},
		{ID: "unresolved", Action: models.ActionAllow},
	}}, {Address: otherGuest}})

	for _, tc := range []struct {
		name string
		flow netstack.Flow
		want netstack.Verdict
	}{
		{"the floor before any rule", flow(judgedGuest, "tcp", "10.0.0.5:8080"), netstack.Verdict{Rule: RulePrivate}},
		{"the first matching rule denies", flow(judgedGuest, "tcp", "203.0.113.7:25"), netstack.Verdict{Rule: "no-smtp"}},
		{"a later rule allows the prefix", flow(judgedGuest, "tcp", "203.0.113.7:5432"), netstack.Verdict{Allow: true, Rule: "mail"}},
		{"the protocol is part of the match", flow(judgedGuest, "udp", "203.0.113.7:5432"), netstack.Verdict{Rule: RuleDefault}},
		{"a port set matches its port alone", flow(judgedGuest, "udp", "198.51.100.1:123"), netstack.Verdict{Allow: true, Rule: "ntp"}},
		{"nothing matches", flow(judgedGuest, "udp", "198.51.100.1:124"), netstack.Verdict{Rule: RuleDefault}},
		{"a chain without a policy allows", flow(otherGuest, "tcp", "198.51.100.1:22"), netstack.Verdict{Allow: true, Rule: RuleNone}},
		{"no chain allows", flow(netip.MustParseAddr("10.200.0.4"), "tcp", "198.51.100.1:22"), netstack.Verdict{Allow: true, Rule: RuleNone}},
		{"no chain keeps the floor", flow(netip.MustParseAddr("10.200.0.4"), "tcp", "192.168.1.1:22"), netstack.Verdict{Rule: RulePrivate}},
	} {
		if got := j.Judge(tc.flow); got != tc.want {
			t.Errorf("%s: %+v got %+v, want %+v", tc.name, tc.flow, got, tc.want)
		}
	}

	// An apply replaces every chain, so a policy detached leaves the guest with the floor alone.
	j.Apply(nil)
	if got := j.Judge(flow(judgedGuest, "tcp", "203.0.113.7:25")); got != (netstack.Verdict{Allow: true, Rule: RuleNone}) {
		t.Errorf("after the policy went: %+v", got)
	}
}

type chainsFn func(context.Context) ([]Chain, error)

func (f chainsFn) Chains(ctx context.Context) ([]Chain, error) { return f(ctx) }

// Addresses compile the chains into the judge on every apply, and a source that fails keeps the apply from claiming it did.
func TestAddressesApplyTheChainsToTheJudge(t *testing.T) {
	chains := []Chain{{Address: netip.MustParseAddr("10.200.0.2"), Policy: true}}
	var fail error
	source := chainsFn(func(context.Context) ([]Chain, error) { return chains, fail })
	a, err := NewAddresses(Config{Root: t.TempDir(), Subnet: netip.MustParsePrefix("10.200.0.0/29"), Egress: source})
	if err != nil {
		t.Fatal(err)
	}

	f := flow(netip.MustParseAddr("10.200.0.2"), "tcp", "203.0.113.7:25")
	if got := a.Judge(f); !got.Allow {
		t.Fatalf("before any apply the judge holds no chain, got %+v", got)
	}
	if _, err := a.Allocate(t.Context(), "sb-1"); err != nil {
		t.Fatal(err)
	}
	if got := a.Judge(f); got != (netstack.Verdict{Rule: RuleDefault}) {
		t.Fatalf("after the allocate the chain is on, got %+v", got)
	}

	chains = nil
	if err := a.Reapply(t.Context(), "sb-1"); err != nil {
		t.Fatal(err)
	}
	if got := a.Judge(f); !got.Allow {
		t.Fatalf("after the policy went, got %+v", got)
	}

	fail = errors.New("the policy store is unreadable")
	if err := a.ReapplyAll(t.Context()); err == nil || !errors.Is(err, fail) {
		t.Fatalf("a failed compile was not reported: %v", err)
	}
}
