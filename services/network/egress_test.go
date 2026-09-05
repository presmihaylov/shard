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
	chains []Chain
	err    error
}

func (f fakeEgress) Chains(context.Context) ([]Chain, error) { return f.chains, f.err }

// A sandbox with no policy keeps the internet and loses everything private, and one with a policy
// runs its own chain and nothing else. The inet forward hook sees the bridge and not the port, so the
// address picks the chain, and the bridge table is where the port pins the address.
func TestTheRulesetGivesEveryPolicyItsOwnChain(t *testing.T) {
	s := newService(t, Config{})

	got := s.ruleset([]Chain{{
		Address: netip.MustParseAddr("10.87.0.2"),
		Policy:  true,
		Rules: []Compiled{
			{Action: models.ActionAllow, Protocol: "tcp", Ports: []int{80, 443}, Prefixes: []netip.Prefix{netip.MustParsePrefix("93.184.216.34/32")}},
			{Action: models.ActionDeny, Prefixes: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}},
			{Action: models.ActionAllow, Protocol: "udp", Ports: []int{53}},
		},
	}, {
		Address: netip.MustParseAddr("10.87.0.3"),
	}}, []netip.Addr{netip.MustParseAddr("10.87.0.2"), netip.MustParseAddr("10.87.0.3")})

	for _, want := range []string{
		"type filter hook forward priority filter; policy accept;",
		`iifname "shard0" ct state established,related accept`,
		`oifname "shard0" ct state new drop`,
		`iifname "shard0" jump egress`,
		`oifname "shard0" drop`,
		"ip saddr 10.87.0.2 jump egress_shardv2",
		"type nat hook prerouting priority dstnat; policy accept;\n\t\tiifname \"shard0\" ip saddr 10.87.0.2 tcp dport 80 dnat ip to 10.87.0.1:30080\n\t\tiifname \"shard0\" ip saddr 10.87.0.2 tcp dport 443 dnat ip to 10.87.0.1:30443",
		"iifname \"shard0\" ip saddr 10.87.0.3 tcp dport 80 dnat ip to 10.87.0.1:30080",
		"iifname \"shard0\" ip saddr 10.87.0.3 ip daddr 10.87.0.1 tcp dport { 30080, 30443 } accept\n\t\tiifname \"shard0\" drop",
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

	// A sandbox fronted by a secret alone keeps the internet, so it gets no chain and no jump.
	if strings.Contains(got, "egress_shardv3") {
		t.Errorf("the secret-only sandbox got a chain:\n%s", got)
	}
}

func TestEnsureRefusesAChainOutsideTheSubnet(t *testing.T) {
	s := newService(t, Config{Egress: fakeEgress{chains: []Chain{{Address: netip.MustParseAddr("192.168.1.1")}}}})

	_, err := s.chains(t.Context())
	if err == nil || !strings.Contains(err.Error(), "outside the sandbox subnet") {
		t.Errorf("chains = %v", err)
	}
}

func TestEnsureFailsWhenThePoliciesDoNotCompile(t *testing.T) {
	s := newService(t, Config{Egress: fakeEgress{err: errors.New("no resolver")}})

	if _, err := s.chains(t.Context()); err == nil {
		t.Error("a source that failed still yielded a ruleset, which would be applied without the policies")
	}
}
