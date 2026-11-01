package network

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

type fakeLinks struct {
	linked []Linked
	err    error
}

func (f fakeLinks) Linked(context.Context) ([]Linked, error) { return f.linked, f.err }

var sandboxSubnet = netip.MustParsePrefix(DefaultSubnet)

// A link is caught before the proxy's dnat and let in before the bridge's drop, and only from its own source.
func TestTheRulesetTurnsALinkToTheForwarder(t *testing.T) {
	agent := netip.MustParseAddr("10.87.0.2")
	other := netip.MustParseAddr("10.87.0.3")
	links := []Linked{{Source: agent, Targets: []netip.Addr{netip.MustParseAddr("10.86.0.1"), netip.MustParseAddr("10.86.0.2")}}}

	got := newService(t, Config{}).ruleset([]Chain{{Address: agent}}, links, []netip.Addr{agent, other})

	dnat := "iifname \"shard0\" ip saddr 10.87.0.2 ip daddr { 10.86.0.1, 10.86.0.2 } meta l4proto tcp dnat ip to 10.87.0.1:30022\n\t\tiifname \"shard0\" ip saddr 10.87.0.2 tcp dport 80 dnat ip to 10.87.0.1:30080"
	accept := "iifname \"shard0\" ct state established,related accept\n" +
		"\t\tiifname \"shard0\" ip saddr 10.87.0.2 ip daddr 10.87.0.1 tcp dport 30022 ct status dnat ct count over 1024 limit rate 2/second burst 10 packets log prefix \"shard-egress rule=limit \"\n" +
		"\t\tiifname \"shard0\" ip saddr 10.87.0.2 ip daddr 10.87.0.1 tcp dport 30022 ct status dnat ct count over 1024 drop\n" +
		"\t\tiifname \"shard0\" ip saddr 10.87.0.2 ip daddr 10.87.0.1 tcp dport 30022 ct status dnat accept\n"
	for _, want := range []string{dnat, accept} {
		if !strings.Contains(got, want) {
			t.Errorf("the ruleset has no %q:\n%s", want, got)
		}
	}

	if strings.Index(got, accept) > strings.Index(got, "iifname \"shard0\" drop") {
		t.Errorf("the link accept comes after the bridge drop:\n%s", got)
	}
	if strings.Contains(got, "ip saddr 10.87.0.3 ip daddr { 10.86") || strings.Contains(got, "ip saddr 10.87.0.3 ip daddr 10.87.0.1 tcp dport 30022") {
		t.Errorf("a sandbox with no link reaches the forwarder:\n%s", got)
	}
	// The floor still drops what no link rule turned: a routed flow into the link range is private.
	if !strings.Contains(got, "ip daddr { 10.0.0.0/8,") {
		t.Errorf("the private floor is gone:\n%s", got)
	}
}

// nft refuses an empty set, so a source whose links all went renders nothing.
func TestTheRulesetSkipsASourceWithNoTargets(t *testing.T) {
	got := newService(t, Config{}).ruleset(nil, []Linked{{Source: netip.MustParseAddr("10.87.0.2")}}, nil)

	if strings.Contains(got, "30022") {
		t.Errorf("a source with no targets got a rule:\n%s", got)
	}
}

func TestLinksRefusesWhatTheRulesCouldNotScope(t *testing.T) {
	for name, linked := range map[string]Linked{
		"source off the bridge":  {Source: netip.MustParseAddr("192.168.1.2"), Targets: []netip.Addr{netip.MustParseAddr("10.86.0.1")}},
		"target off the range":   {Source: netip.MustParseAddr("10.87.0.2"), Targets: []netip.Addr{netip.MustParseAddr("10.87.0.3")}},
		"source with no address": {Targets: []netip.Addr{netip.MustParseAddr("10.86.0.1")}},
		"target with no address": {Source: netip.MustParseAddr("10.87.0.2"), Targets: []netip.Addr{{}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readLinks(t.Context(), fakeLinks{linked: []Linked{linked}}, sandboxSubnet); err == nil {
				t.Error("links accepted a link the rules could not scope")
			}
		})
	}
}

func TestLinksFailsWhenTheSourceDoes(t *testing.T) {
	cause := errors.New("records unreadable")

	if _, err := readLinks(t.Context(), fakeLinks{err: cause}, sandboxSubnet); !errors.Is(err, cause) {
		t.Errorf("links = %v, want the source's error", err)
	}
}

func TestNoLinkSourceIsNoLink(t *testing.T) {
	linked, err := readLinks(t.Context(), nil, sandboxSubnet)
	if err != nil || linked != nil {
		t.Errorf("links = %v, %v; want none", linked, err)
	}
}
