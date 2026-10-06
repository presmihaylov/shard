package network

import (
	"context"
	"errors"
	"fmt"
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
			{ID: "2", Sum: "0123456789ab", Action: models.ActionDeny, Prefixes: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}},
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
		"type nat hook prerouting priority dstnat; policy accept;\n\t\tiifname \"shard0\" ip saddr 10.87.0.2 tcp dport 80 dnat ip to 10.87.0.1:30080\n\t\tiifname \"shard0\" ip saddr 10.87.0.2 tcp dport 443 dnat ip to 10.87.0.1:30443\n\t\tiifname \"shard0\" ip saddr 10.87.0.2 udp dport 53 dnat ip to 10.87.0.1:53\n\t\tiifname \"shard0\" ip saddr 10.87.0.2 tcp dport 53 dnat ip to 10.87.0.1:53",
		"iifname \"shard0\" ip saddr 10.87.0.3 tcp dport 80 dnat ip to 10.87.0.1:30080",
		"oifname \"shard0\" drop\n\t\tmeta nfproto ipv6 limit rate 2/second burst 10 packets log prefix \"shard-egress rule=ipv6 \"\n\t\tmeta nfproto ipv6 drop",
		"iifname \"shard0\" ip saddr 10.87.0.2 ip daddr 10.87.0.1 tcp dport { 30080, 30443 } ct count over 1024 drop\n\t\tiifname \"shard0\" ip saddr 10.87.0.2 ip daddr 10.87.0.1 tcp dport { 30080, 30443 } accept",
		"iifname \"shard0\" ip saddr 10.87.0.3 ip daddr 10.87.0.1 tcp dport { 30080, 30443 } ct count over 1024 limit rate 2/second burst 10 packets log prefix \"shard-egress rule=limit \"\n\t\tiifname \"shard0\" ip saddr 10.87.0.3 ip daddr 10.87.0.1 tcp dport { 30080, 30443 } ct count over 1024 drop\n\t\tiifname \"shard0\" ip saddr 10.87.0.3 ip daddr 10.87.0.1 tcp dport { 30080, 30443 } accept\n\t\tiifname \"shard0\" ip daddr 10.87.0.1 udp dport 53 accept\n\t\tiifname \"shard0\" ip daddr 10.87.0.1 tcp dport 53 accept\n\t\tiifname \"shard0\" ip saddr 10.87.0.2 limit rate 2/second burst 10 packets log prefix \"shard-egress rule=local \"\n\t\tiifname \"shard0\" ip saddr 10.87.0.3 limit rate 2/second burst 10 packets log prefix \"shard-egress rule=local \"\n\t\tiifname \"shard0\" drop",
		"table bridge shard\ndelete table bridge shard",
		"table bridge shard {\n\tchain forward {\n\t\ttype filter hook forward priority filter; policy accept;\n\t\tmeta ibrname \"shard0\" drop\n\t}",
		"type filter hook prerouting priority filter; policy accept;\n\t\tiifname \"shardv2\" ether type ip6 limit rate 2/second burst 10 packets log prefix \"shard-egress rule=ipv6 \"\n\t\tiifname \"shardv2\" ether type ip6 drop\n\t\tiifname \"shardv2\" ether type ip ip saddr != 10.87.0.2 drop\n\t\tiifname \"shardv2\" arp saddr ip != 10.87.0.2 drop",
		"iifname \"shardv3\" ether type ip ip saddr != 10.87.0.3 drop",
		"ip saddr 10.87.0.2 ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16, 127.0.0.0/8, 100.64.0.0/10 } limit rate 2/second burst 10 packets log prefix \"shard-egress rule=private \"\n\t\tip saddr 10.87.0.3 ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16, 127.0.0.0/8, 100.64.0.0/10 } limit rate 2/second burst 10 packets log prefix \"shard-egress rule=private \"\n\t\tip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16, 127.0.0.0/8, 100.64.0.0/10 } drop\n\t\tip saddr 10.87.0.2 jump egress_shardv2",
		"chain egress_shardv2 {\n\t\tip daddr { 93.184.216.34/32 } meta l4proto tcp tcp dport { 80, 443 } accept\n\t\tip daddr { 0.0.0.0/0 } limit rate 2/second burst 10 packets log prefix \"shard-egress rule=2 sum=0123456789ab \"\n\t\tip daddr { 0.0.0.0/0 } drop\n\t\t# no address\n\t\tlimit rate 2/second burst 10 packets log prefix \"shard-egress rule=default \"\n\t\tdrop\n\t}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the ruleset has no %q:\n%s", want, got)
		}
	}

	// A sandbox fronted by a secret alone keeps the internet, so it gets no chain, no jump and no resolver.
	if strings.Contains(got, "egress_shardv3") {
		t.Errorf("the secret-only sandbox got a chain:\n%s", got)
	}
	if strings.Contains(got, "ip saddr 10.87.0.3 udp dport 53") || strings.Contains(got, "ip saddr 10.87.0.3 tcp dport 53") {
		t.Errorf("the secret-only sandbox had its DNS caught:\n%s", got)
	}
}

// SHARD-628: a shared limit let one sandbox probing the floor spend every other sandbox's drop records.
func TestEverySandboxLogsTheFloorUnderItsOwnLimit(t *testing.T) {
	leases := []netip.Addr{netip.MustParseAddr("10.87.0.2"), netip.MustParseAddr("10.87.0.3")}
	got := newService(t, Config{}).ruleset(nil, leases)

	for _, rule := range []string{RuleLocal, RulePrivate} {
		checkFloorLog(t, got, rule, leases)
	}
}

// checkFloorLog wants one log rule for the floor rule per lease, in lease order, each naming its sandbox.
func checkFloorLog(t *testing.T, ruleset, rule string, leases []netip.Addr) {
	t.Helper()

	var lines []string
	for line := range strings.Lines(ruleset) {
		if strings.Contains(line, "rule="+rule+" ") {
			lines = append(lines, line)
		}
	}
	if len(lines) != len(leases) {
		t.Errorf("rule=%s has %d log rules, want one per sandbox:\n%s", rule, len(lines), ruleset)
		return
	}
	for i, address := range leases {
		if !strings.Contains(lines[i], "ip saddr "+address.String()+" ") {
			t.Errorf("rule=%s log rule %q does not name sandbox %s", rule, lines[i], address)
		}
	}
}

func TestEnsureRefusesAChainOutsideTheSubnet(t *testing.T) {
	s := newService(t, Config{Egress: fakeEgress{chains: []Chain{{Address: netip.MustParseAddr("192.168.1.1")}}}})

	_, _, err := s.chains(t.Context())
	if err == nil || !strings.Contains(err.Error(), "outside the sandbox subnet") {
		t.Errorf("chains = %v", err)
	}
}

func TestEnsureFailsWhenThePoliciesDoNotCompile(t *testing.T) {
	s := newService(t, Config{Egress: fakeEgress{err: errors.New("no resolver")}})

	if _, _, err := s.chains(t.Context()); err == nil {
		t.Error("a source that failed still yielded a ruleset, which would be applied without the policies")
	}
}

func TestChainsKeepAHeldCompileAndItsChains(t *testing.T) {
	held := &HeldChains{Errs: map[string]error{"sb-2": errors.New("no such host")}}
	source := fakeEgress{chains: []Chain{{Address: netip.MustParseAddr("10.87.0.2")}, {Address: netip.MustParseAddr("10.87.0.3"), Policy: true}}, err: held}
	s := newService(t, Config{Egress: source})

	chains, got, err := s.chains(t.Context())
	if err != nil || got != held || len(chains) != 2 {
		t.Errorf("chains = %+v, %v, %v; want both chains and the held error", chains, got, err)
	}
}

func TestHeldForFailsOnlyTheSandboxItHeld(t *testing.T) {
	cause := errors.New("no such host")
	held := &HeldChains{Errs: map[string]error{"sb-2": cause}}

	if err := heldFor(held, "sb-1"); err != nil {
		t.Errorf("heldFor(sb-1) = %v, want nil", err)
	}
	if err := heldFor(held, "sb-2"); !errors.Is(err, cause) {
		t.Errorf("heldFor(sb-2) = %v, want its cause", err)
	}
	whole := errors.New("the policy store is unreadable")
	if err := heldFor(whole, "sb-1"); !errors.Is(err, whole) {
		t.Errorf("heldFor(a whole failure) = %v, want it kept", err)
	}
}

func TestHeldReportedSwallowsOnlyAHeldCompile(t *testing.T) {
	var lines []string
	report := func(format string, v ...any) { lines = append(lines, fmt.Sprintf(format, v...)) }
	held := &HeldChains{Errs: map[string]error{"sb-2": errors.New("resolve gone.example.com: no such host")}}

	if err := heldReported(held, report); err != nil || len(lines) != 1 || !strings.Contains(lines[0], "sandbox sb-2: resolve gone.example.com") {
		t.Errorf("heldReported = %v, lines %q", err, lines)
	}
	whole := errors.New("the policy store is unreadable")
	if err := heldReported(whole, report); !errors.Is(err, whole) || len(lines) != 1 {
		t.Errorf("a whole failure was reported, not returned: %v, lines %q", err, lines)
	}
	if err := heldReported(nil, report); err != nil || len(lines) != 1 {
		t.Errorf("a clean apply reported %q", lines)
	}
}
