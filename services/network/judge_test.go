package network

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

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

	// The host's own addresses are refused as local, wherever they sit, and so is a lookup that fails.
	j.Local = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("203.0.113.9")}, nil }
	j.hostRead = time.Time{}
	for _, tc := range []struct {
		name string
		flow netstack.Flow
		want netstack.Verdict
	}{
		{"an address the host owns", flow(judgedGuest, "tcp", "203.0.113.9:5432"), netstack.Verdict{Rule: RuleLocal}},
		{"the unspecified address", flow(judgedGuest, "tcp", "0.0.0.0:5432"), netstack.Verdict{Rule: RuleLocal}},
		{"broadcast", flow(judgedGuest, "udp", "255.255.255.255:123"), netstack.Verdict{Rule: RuleLocal}},
		{"multicast", flow(judgedGuest, "udp", "224.0.0.251:5353"), netstack.Verdict{Rule: RuleLocal}},
		{"the same prefix, not the host's", flow(judgedGuest, "tcp", "203.0.113.7:5432"), netstack.Verdict{Allow: true, Rule: "mail"}},
	} {
		if got := j.Judge(tc.flow); got != tc.want {
			t.Errorf("%s: %+v got %+v, want %+v", tc.name, tc.flow, got, tc.want)
		}
	}
	j.Local = func() ([]netip.Addr, error) { return nil, errors.New("no interfaces") }
	j.hostRead = time.Time{}
	if got := j.Judge(flow(judgedGuest, "tcp", "203.0.113.7:5432")); got != (netstack.Verdict{Rule: RuleLocal}) {
		t.Errorf("a failed lookup allowed: %+v", got)
	}
	j.Local = nil
	j.hostRead = time.Time{}

	// An apply replaces every chain, so a policy detached leaves the guest with the floor alone.
	j.Apply(nil)
	if got := j.Judge(flow(judgedGuest, "tcp", "203.0.113.7:25")); got != (netstack.Verdict{Allow: true, Rule: RuleNone}) {
		t.Errorf("after the policy went: %+v", got)
	}
}

// The zero judge refuses every judged flow until its first apply, so VMs adopted before it open no window.
func TestTheZeroJudgeRefusesUntilTheFirstApply(t *testing.T) {
	var j Judge
	f := flow(judgedGuest, "tcp", "203.0.113.7:25")
	if got := j.Judge(f); got != (netstack.Verdict{Rule: RuleUnapplied}) {
		t.Fatalf("before the first apply: %+v", got)
	}
	j.Apply(nil)
	if got := j.Judge(f); got != (netstack.Verdict{Allow: true, Rule: RuleNone}) {
		t.Fatalf("after an apply with no chain: %+v", got)
	}
}

// Two applies land in the order they compiled, so a stalled older compile never overwrites a newer policy.
func TestAStalledReapplyNeverLandsOverANewerOne(t *testing.T) {
	guest := netip.MustParseAddr("10.200.0.2")
	var calls atomic.Int32
	stall := make(chan struct{})
	source := chainsFn(func(context.Context) ([]Chain, error) {
		// The first compile stalls without the policy; the second has it, and without the lock would be overwritten.
		if calls.Add(1) == 1 {
			<-stall

			return []Chain{{Address: guest}}, nil
		}

		return []Chain{{Address: guest, Policy: true}}, nil
	})
	a, err := NewAddresses(Config{Root: t.TempDir(), Subnet: netip.MustParsePrefix("10.200.0.0/29"), Egress: source})
	if err != nil {
		t.Fatal(err)
	}

	first := make(chan error, 1)
	go func() { first <- a.ReapplyAll(t.Context()) }()
	second := make(chan error, 1)
	go func() { second <- a.Reapply(t.Context(), "sb-1") }()
	select {
	case err := <-second:
		t.Fatalf("the second apply landed before the first: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(stall)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if got := a.Judge(flow(guest, "tcp", "203.0.113.7:25")); got != (netstack.Verdict{Rule: RuleDefault}) {
		t.Fatalf("the newer policy is not what the judge holds: %+v", got)
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
	if got := a.Judge(f); got != (netstack.Verdict{Rule: RuleUnapplied}) {
		t.Fatalf("before any apply the judge refuses, got %+v", got)
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
