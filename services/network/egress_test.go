package network

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

type fakeEgress struct {
	chains  []Chain
	fronted []netip.Addr
	err     error
}

func (f fakeEgress) Chains(context.Context) ([]Chain, error)       { return f.chains, f.err }
func (f fakeEgress) Fronted(context.Context) ([]netip.Addr, error) { return f.fronted, f.err }

// A sandbox with no policy keeps the internet and loses everything private, and one with a policy
// runs its own chain and nothing else. The inet forward hook sees the bridge and not the port, so the
// address picks the chain, and the bridge table is where the port pins the address.
func TestTheRulesetGivesEveryPolicyItsOwnChain(t *testing.T) {
	s := newService(t, Config{})

	got := s.ruleset([]Chain{{
		Address: netip.MustParseAddr("10.87.0.2"),
		Rules: []Compiled{
			{Action: models.ActionAllow, Protocol: "tcp", Ports: []int{80, 443}, Prefixes: []netip.Prefix{netip.MustParsePrefix("93.184.216.34/32")}},
			{Action: models.ActionDeny, Prefixes: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}},
			{Action: models.ActionAllow, Protocol: "udp", Ports: []int{53}},
		},
	}}, []netip.Addr{netip.MustParseAddr("10.87.0.2"), netip.MustParseAddr("10.87.0.3")}, nil)

	for _, want := range []string{
		"type filter hook forward priority filter; policy accept;",
		`iifname "shard0" ct state established,related accept`,
		`oifname "shard0" ct state new drop`,
		`iifname "shard0" jump egress`,
		`oifname "shard0" drop`,
		"ip saddr 10.87.0.2 jump egress_shardv2",
		"table bridge shard\ndelete table bridge shard",
		"table bridge shard {\n\tchain forward {\n\t\ttype filter hook forward priority filter; policy accept;\n\t\tmeta ibrname \"shard0\" drop\n\t}",
		"type filter hook prerouting priority filter; policy accept;\n\t\tiifname \"shardv2\" ether type ip ip saddr != 10.87.0.2 drop\n\t\tiifname \"shardv2\" arp saddr ip != 10.87.0.2 drop",
		"iifname \"shardv3\" ether type ip ip saddr != 10.87.0.3 drop",
		"oifname \"shard0\" drop\n\t\tip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16, 127.0.0.0/8, 100.64.0.0/10 } drop\n\t\tip saddr 10.87.0.2 jump egress_shardv2",
		"chain egress_shardv2 {\n\t\tip daddr { 93.184.216.34/32 } meta l4proto tcp tcp dport { 80, 443 } accept\n\t\tip daddr { 0.0.0.0/0 } drop\n\t\t# no address\n\t\tdrop\n\t}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the ruleset has no %q:\n%s", want, got)
		}
	}
}

// A fronted sandbox's 80 and 443 are turned to the proxy before routing, and the proxy's ports are the
// only thing on the host it may reach. A sandbox nobody fronts keeps going straight out.
func TestTheRulesetSendsAFrontedSandboxToTheProxy(t *testing.T) {
	s := newService(t, Config{})

	got := s.ruleset(nil, nil, []netip.Addr{netip.MustParseAddr("10.87.0.2")})

	for _, want := range []string{
		"type nat hook prerouting priority dstnat; policy accept;",
		`iifname "shard0" ip saddr 10.87.0.2 tcp dport 80 dnat to 10.87.0.1:18080`,
		`iifname "shard0" ip saddr 10.87.0.2 tcp dport 443 dnat to 10.87.0.1:18443`,
		"ct state established,related accept\n\t\tiifname \"shard0\" ip saddr 10.87.0.2 ip daddr 10.87.0.1 tcp dport { 18080, 18443 } accept\n\t\tiifname \"shard0\" drop",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the ruleset has no %q:\n%s", want, got)
		}
	}

	if plain := s.ruleset(nil, nil, nil); strings.Contains(plain, "dnat") || strings.Contains(plain, "18080") {
		t.Errorf("a ruleset with nothing fronted still names the proxy:\n%s", plain)
	}
}

func TestEnsureRefusesAnAddressOutsideTheSubnet(t *testing.T) {
	outside := netip.MustParseAddr("192.168.1.1")

	s := newService(t, Config{Egress: fakeEgress{chains: []Chain{{Address: outside}}}})
	if _, _, err := s.egress(t.Context()); err == nil || !strings.Contains(err.Error(), "outside the sandbox subnet") {
		t.Errorf("a chain outside the subnet: %v", err)
	}

	s = newService(t, Config{Egress: fakeEgress{fronted: []netip.Addr{outside}}})
	if _, _, err := s.egress(t.Context()); err == nil || !strings.Contains(err.Error(), "outside the sandbox subnet") {
		t.Errorf("a fronted address outside the subnet: %v", err)
	}
}

func TestEnsureFailsWhenThePoliciesDoNotCompile(t *testing.T) {
	s := newService(t, Config{Egress: fakeEgress{err: errors.New("no resolver")}})

	if _, _, err := s.egress(t.Context()); err == nil {
		t.Error("a source that failed still yielded a ruleset, which would be applied without the policies")
	}
}
