package egress

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

func effective(rules ...EffectiveRule) Effective {
	return Effective{Policy: "locked", Rules: rules}
}

func explicit(action models.Action, kind models.DestinationKind, value string) EffectiveRule {
	return EffectiveRule{Rule: models.Rule{Action: action, Destination: models.Destination{Kind: kind, Value: value}, Protocol: "tcp"}}
}

var (
	public  = netip.MustParseAddr("93.184.216.34")
	private = netip.MustParseAddr("10.0.0.5")
)

// The proxy sees a name, the chain sees an address; Decide must read a policy the same way the chain does.
func TestDecideRunsTheRulesInOrderByNameAndAddress(t *testing.T) {
	eff := effective(
		explicit(models.ActionDeny, models.DestinationDomain, "bad.example.com"),
		explicit(models.ActionAllow, models.DestinationDomainSuffix, "example.com"),
		explicit(models.ActionAllow, models.DestinationCIDR, "1.1.1.1"),
		explicit(models.ActionDeny, models.DestinationGroup, "any"),
	)

	for _, tc := range []struct {
		host  string
		addr  netip.Addr
		allow bool
		rule  string
	}{
		{"bad.example.com", public, false, "bad.example.com"},
		{"api.example.com", public, true, "example.com"},
		{"example.com", public, true, "example.com"},
		{"notexample.com", public, false, "any"},
		{"one.one.one.one", netip.MustParseAddr("1.1.1.1"), true, "1.1.1.1"},
		{"other.net", public, false, "any"},
	} {
		got := Decide(eff, tc.host, 443, tc.addr)
		if got.Allow != tc.allow || got.Rule == nil || got.Rule.Destination.Value != tc.rule {
			t.Errorf("Decide(%s) = %+v, want allow=%v by %s", tc.host, got, tc.allow, tc.rule)
		}
	}
}

func TestDecideDropsWhatNoRuleNamesAndAMissingPolicy(t *testing.T) {
	got := Decide(effective(explicit(models.ActionAllow, models.DestinationDomain, "api.example.com")), "other.example.com", 443, public)
	if got.Allow || got.Rule != nil || !strings.Contains(got.Reason, "no rule for other.example.com:443") {
		t.Errorf("an unmatched host got %+v", got)
	}

	got = Decide(Effective{Policy: "gone", Missing: true}, "api.example.com", 443, public)
	if got.Allow || !strings.Contains(got.Reason, "no longer exists") {
		t.Errorf("a missing policy got %+v", got)
	}
}

func TestDecideMatchesTheProtocolAndThePorts(t *testing.T) {
	udpOnly := EffectiveRule{Rule: models.Rule{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationGroup, Value: "any"}, Protocol: "udp"}}
	port80 := EffectiveRule{Rule: models.Rule{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationGroup, Value: "any"}, Protocol: "tcp", Ports: []int{80}}}

	if got := Decide(effective(udpOnly), "api.example.com", 443, public); got.Allow {
		t.Errorf("a udp rule matched a tcp request: %+v", got)
	}
	if got := Decide(effective(port80), "api.example.com", 443, public); got.Allow {
		t.Errorf("a port 80 rule matched port 443: %+v", got)
	}
	if got := Decide(effective(port80), "api.example.com", 80, public); !got.Allow {
		t.Errorf("a port 80 rule missed port 80: %+v", got)
	}
}

// A sandbox with no policy reaches the internet and nothing private, at the proxy as in the chain.
func TestDecideWithoutAPolicyKeepsTheHostToItself(t *testing.T) {
	if got := Decide(Effective{}, "api.example.com", 443, public); !got.Allow {
		t.Errorf("no policy denied a public host: %+v", got)
	}
	if got := Decide(Effective{}, "internal.example.com", 443, private); got.Allow || !strings.Contains(got.Reason, "private") {
		t.Errorf("no policy allowed a private address: %+v", got)
	}
}

// A grant opens its host, and a host that resolves inward is not that host.
func TestDecideRefusesAGrantedHostThatResolvesToAPrivateAddress(t *testing.T) {
	granted := explicit(models.ActionAllow, models.DestinationDomain, "api.example.com")
	granted.Implied = "secret TOKEN"

	if got := Decide(effective(granted), "api.example.com", 443, public); !got.Allow {
		t.Errorf("the grant did not open its host: %+v", got)
	}
	if got := Decide(effective(granted), "api.example.com", 443, private); got.Allow || !strings.Contains(got.Reason, "private") {
		t.Errorf("the grant opened a private address: %+v", got)
	}

	// Written into the policy by the operator, the same rule does open it.
	if got := Decide(effective(explicit(models.ActionAllow, models.DestinationDomain, "api.example.com")), "api.example.com", 443, private); !got.Allow {
		t.Errorf("an explicit rule did not open a private address: %+v", got)
	}
}

// A stopped sandbox keeps its lease, so it keeps its place at the proxy: a start finds the turn in place.
func TestFrontedListsTheSandboxesWithAGrantOrAPolicy(t *testing.T) {
	addr := func(last int) netip.Prefix { return netip.MustParsePrefix("10.87.0." + string(rune('0'+last)) + "/16") }
	s := New(newStore(t), fakeRecords{
		{ID: "grant", State: models.StateRunning, Secrets: []string{"TOKEN"}, Address: addr(2)},
		{ID: "policy", State: models.StateRunning, Policy: "locked", Address: addr(3)},
		{ID: "plain", State: models.StateRunning, Address: addr(4)},
		{ID: "stopped", State: models.StateStopped, Policy: "locked", Address: addr(5)},
		{ID: "unaddressed", State: models.StateRunning, Policy: "locked"},
	}, fakeGrants{}, nil, fakeResolver{})

	got, err := s.Fronted(t.Context())
	if err != nil {
		t.Fatalf("Fronted: %v", err)
	}

	want := []netip.Addr{addr(2).Addr(), addr(3).Addr(), addr(5).Addr()}
	if !slices.Equal(got, want) {
		t.Errorf("Fronted = %v, want %v", got, want)
	}
}

func TestMatchHostFollowsTheWildcardTable(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		{"example.com", "example.com", true},
		{"example.com", "www.example.com", false},
		{"*.example.com", "www.example.com", true},
		{"*.example.com", "www.api.example.com", true},
		{"*.example.com", "example.com", false},
		{"www.*.com", "www.example.com", true},
		{"www.*.com", "example.com", false},
		{"www.*.com", "www.api.example.com", false},
		{"*", "anything.at.all", true},
		{"*.example.com", "wwwexample.com", false},
	}
	for _, c := range cases {
		if got := MatchHost(c.pattern, c.host); got != c.want {
			t.Errorf("MatchHost(%q, %q) = %v, want %v", c.pattern, c.host, got, c.want)
		}
	}
}

func TestDecideMatchesAWildcardDomainRule(t *testing.T) {
	eff := Effective{Policy: "p", Rules: []EffectiveRule{
		{Rule: models.Rule{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "*.example.com"}, Protocol: "tcp", Ports: []int{80, 443}}},
		{Rule: models.Rule{Action: models.ActionDeny, Destination: models.Destination{Kind: models.DestinationGroup, Value: "any"}}},
	}}

	decision := Decide(eff, "www.example.com", 443, netip.MustParseAddr("93.184.216.34"))
	if !decision.Allow || decision.ID != "0" {
		t.Errorf("the wildcard did not allow www.example.com: %+v", decision)
	}
	decision = Decide(eff, "example.com", 443, netip.MustParseAddr("93.184.216.34"))
	if decision.Allow {
		t.Errorf("the wildcard allowed the apex: %+v", decision)
	}
}
