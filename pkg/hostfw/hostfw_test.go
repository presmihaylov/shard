package hostfw_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/pkg/hostfw"
)

var hole = hostfw.Hole{
	Name:      "shard",
	Marker:    "managed-by-shard",
	Interface: "shard0",
	Policy:    "shard-forwarding",
	Rules: []hostfw.Rule{
		{Chain: "INPUT", Match: []string{"-d", "10.87.0.1/32", "-i", "shard0", "-p", "udp", "-m", "udp", "--dport", "53"}},
		{Chain: "FORWARD", Match: []string{"-i", "shard0"}},
	},
}

const (
	inputLine   = "-A INPUT -d 10.87.0.1/32 -i shard0 -p udp -m udp --dport 53 -m comment --comment managed-by-shard -j ACCEPT"
	forwardLine = "-A FORWARD -i shard0 -m comment --comment managed-by-shard -j ACCEPT"
	open        = "-P INPUT ACCEPT\n-P FORWARD ACCEPT\n-P OUTPUT ACCEPT\n"
	ufw         = "-P INPUT DROP\n-P FORWARD DROP\n-P OUTPUT ACCEPT\n-N ufw-before-input\n-A INPUT -j ufw-before-input\n-A FORWARD -j ufw-before-forward\n"
)

// exitError is a failed command's exit status, as *exec.ExitError carries it.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func (e exitError) ExitCode() int { return int(e) }

type answer struct {
	out  string
	code int
}

// fakeHost answers each command line it knows and passes any other with no output, recording every call.
type fakeHost struct {
	answers map[string]answer
	calls   []string
}

func (f *fakeHost) run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	a := f.answers[call]
	if a.code != 0 {
		return []byte(a.out), fmt.Errorf("%s: %w", call, exitError(a.code))
	}

	return []byte(a.out), nil
}

func iptablesHost(answers map[string]answer) (*fakeHost, *hostfw.Manager) {
	f := &fakeHost{answers: answers}

	return f, hostfw.New(f.run, "iptables", "")
}

func requireCalls(t *testing.T, f *fakeHost, want ...string) {
	t.Helper()

	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n  %s\nwant:\n  %s", strings.Join(f.calls, "\n  "), strings.Join(want, "\n  "))
	}
}

// A host whose chains drop nothing gets no rule, so the bridge works as it did and nothing new appears.
func TestEnsureRulesLeavesAHostThatDoesNotFilter(t *testing.T) {
	cases := map[string]string{
		"every chain accepts":              open,
		"a chain the hole does not use":    "-P INPUT ACCEPT\n-P FORWARD ACCEPT\n-P OUTPUT DROP\n-A OUTPUT -j DROP\n",
		"only the hole's own rules remain": open + inputLine + "\n",
	}
	for name, listing := range cases {
		t.Run(name, func(t *testing.T) {
			f, m := iptablesHost(map[string]answer{"iptables -w -S": {out: listing}})

			if err := m.EnsureRules(t.Context(), hole); err != nil {
				t.Fatalf("EnsureRules: %v", err)
			}
			requireCalls(t, f, "iptables -w -S")
		})
	}
}

// A chain that drops, by its policy or by a rule of another, gets the hole's rules at the top in the hole's order.
func TestEnsureRulesOpensAChainThatDrops(t *testing.T) {
	cases := map[string]string{
		"ufw":                           ufw,
		"an accept policy, then a drop": "-P INPUT ACCEPT\n-P FORWARD ACCEPT\n-A INPUT -j DROP\n",
	}
	for name, listing := range cases {
		t.Run(name, func(t *testing.T) {
			f, m := iptablesHost(map[string]answer{
				"iptables -w -S": {out: listing},
				"iptables -w -C FORWARD -i shard0 -m comment --comment managed-by-shard -j ACCEPT":                                        {code: 1},
				"iptables -w -C INPUT -d 10.87.0.1/32 -i shard0 -p udp -m udp --dport 53 -m comment --comment managed-by-shard -j ACCEPT": {code: 1},
			})

			if err := m.EnsureRules(t.Context(), hole); err != nil {
				t.Fatalf("EnsureRules: %v", err)
			}
			requireCalls(t, f,
				"iptables -w -S",
				"iptables -w -C FORWARD -i shard0 -m comment --comment managed-by-shard -j ACCEPT",
				"iptables -w -I FORWARD 1 -i shard0 -m comment --comment managed-by-shard -j ACCEPT",
				"iptables -w -C INPUT -d 10.87.0.1/32 -i shard0 -p udp -m udp --dport 53 -m comment --comment managed-by-shard -j ACCEPT",
				"iptables -w -I INPUT 1 -d 10.87.0.1/32 -i shard0 -p udp -m udp --dport 53 -m comment --comment managed-by-shard -j ACCEPT",
			)
		})
	}
}

// The timer's quiet pass is one listing: rules a reload has not flushed are found there and touched no further.
func TestEnsureRulesFindsItsRulesInOneListing(t *testing.T) {
	f, m := iptablesHost(map[string]answer{"iptables -w -S": {out: ufw + inputLine + "\n" + forwardLine + "\n"}})

	if err := m.EnsureRules(t.Context(), hole); err != nil {
		t.Fatalf("EnsureRules: %v", err)
	}
	requireCalls(t, f, "iptables -w -S")
}

// An iptables that prints a rule in another order still holds it, and -C is what says so.
func TestEnsureRulesTrustsTheCheckOverTheListing(t *testing.T) {
	f, m := iptablesHost(map[string]answer{"iptables -w -S": {out: ufw}})

	if err := m.EnsureRules(t.Context(), hole); err != nil {
		t.Fatalf("EnsureRules: %v", err)
	}
	for _, call := range f.calls {
		if strings.Contains(call, " -I ") {
			t.Errorf("EnsureRules inserted %q, which -C found", call)
		}
	}
}

// Only exit status 1 means the rule is missing; any other failure of the check is the caller's to see.
func TestEnsureRulesReturnsAFailedCheck(t *testing.T) {
	f, m := iptablesHost(map[string]answer{
		"iptables -w -S": {out: ufw},
		"iptables -w -C FORWARD -i shard0 -m comment --comment managed-by-shard -j ACCEPT": {out: "Another app is currently holding the xtables lock", code: 4},
	})

	err := m.EnsureRules(t.Context(), hole)
	if err == nil || !strings.Contains(err.Error(), "check the FORWARD chain") {
		t.Fatalf("EnsureRules = %v, want the failed check", err)
	}
	if slices.ContainsFunc(f.calls, func(call string) bool { return strings.Contains(call, " -I ") }) {
		t.Errorf("EnsureRules inserted after a failed check: %q", f.calls)
	}
}

// A host without the tools gets no call at all.
func TestAManagerWithoutToolsDoesNothing(t *testing.T) {
	f := &fakeHost{}
	m := hostfw.New(f.run, "", "")

	for name, call := range map[string]func(context.Context, hostfw.Hole) error{
		"EnsureRules": m.EnsureRules,
		"EnsureZone":  m.EnsureZone,
		"Close":       m.Close,
	} {
		if err := call(t.Context(), hole); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	requireCalls(t, f)
}

// Close drops each rule the marker tags, and leaves the host's own, even one an admin named after shard.
func TestCloseDropsOnlyTheHolesRules(t *testing.T) {
	other := "-A FORWARD -i docker0 -m comment --comment shardish -j ACCEPT\n-A INPUT -p tcp -m tcp --dport 22 -m comment --comment shard -j ACCEPT"
	f, m := iptablesHost(map[string]answer{"iptables -w -S": {out: ufw + inputLine + "\n" + other + "\n" + forwardLine + "\n"}})

	if err := m.Close(t.Context(), hole); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requireCalls(t, f,
		"iptables -w -S",
		"iptables -w -D INPUT -d 10.87.0.1/32 -i shard0 -p udp -m udp --dport 53 -m comment --comment managed-by-shard -j ACCEPT",
		"iptables -w -D FORWARD -i shard0 -m comment --comment managed-by-shard -j ACCEPT",
	)
}

func firewalldHost(answers map[string]answer) (*fakeHost, *hostfw.Manager) {
	f := &fakeHost{answers: answers}

	return f, hostfw.New(f.run, "", "firewall-cmd")
}

const (
	newZone = "shard\n  target: default\n  icmp-block-inversion: no\n  interfaces: \n  sources: \n"
	ourZone = "shard\n  target: ACCEPT\n  icmp-block-inversion: no\n  interfaces: shard0\n  sources: \n"
	newPol  = "shard-forwarding\n  priority: -1\n  target: CONTINUE\n  ingress-zones: \n  egress-zones: \n"
	ourPol  = "shard-forwarding\n  priority: -1\n  target: ACCEPT\n  ingress-zones: shard\n  egress-zones: ANY\n"
)

// withMarks answers that the zone and the policy carry the marker, so they are shard's.
func withMarks(answers map[string]answer) map[string]answer {
	answers["firewall-cmd --permanent --zone=shard --get-description"] = answer{out: "managed-by-shard\n"}
	answers["firewall-cmd --permanent --policy=shard-forwarding --get-description"] = answer{out: "managed-by-shard\n"}

	return answers
}

// firewalld off is a host without it: one question, and nothing else.
func TestEnsureZoneLeavesAStoppedFirewalld(t *testing.T) {
	f, m := firewalldHost(map[string]answer{"firewall-cmd --state": {out: "not running", code: 252}})

	if err := m.EnsureZone(t.Context(), hole); err != nil {
		t.Fatalf("EnsureZone: %v", err)
	}
	requireCalls(t, f, "firewall-cmd --state")
}

// A firewall-cmd that fails for another reason than a stopped daemon is an error, never a host without one.
func TestEnsureZoneReturnsAFailedState(t *testing.T) {
	_, m := firewalldHost(map[string]answer{"firewall-cmd --state": {code: 1}})

	if err := m.EnsureZone(t.Context(), hole); err == nil {
		t.Fatal("EnsureZone = nil, want the failed --state")
	}
}

// On firewalld 1.x the zone takes the interface and accepts it, the policy lets it forward, and one reload applies both.
func TestEnsureZoneCreatesTheZoneAndThePolicy(t *testing.T) {
	f, m := firewalldHost(map[string]answer{
		"firewall-cmd --version":                                  {out: "1.3.4\n"},
		"firewall-cmd --permanent --get-zones":                    {out: "block dmz drop public trusted\n"},
		"firewall-cmd --permanent --info-zone=shard":              {out: newZone},
		"firewall-cmd --permanent --get-policies":                 {out: "allow-host-ipv6\n"},
		"firewall-cmd --permanent --info-policy=shard-forwarding": {out: newPol},
		"firewall-cmd --get-zones":                                {out: "block dmz drop public trusted\n"},
	})

	if err := m.EnsureZone(t.Context(), hole); err != nil {
		t.Fatalf("EnsureZone: %v", err)
	}
	requireCalls(t, f,
		"firewall-cmd --state",
		"firewall-cmd --permanent --get-zones",
		"firewall-cmd --permanent --new-zone=shard",
		"firewall-cmd --permanent --zone=shard --set-description=managed-by-shard",
		"firewall-cmd --permanent --info-zone=shard",
		"firewall-cmd --permanent --zone=shard --set-target=ACCEPT",
		"firewall-cmd --permanent --zone=shard --add-interface=shard0",
		"firewall-cmd --version",
		"firewall-cmd --permanent --get-policies",
		"firewall-cmd --permanent --new-policy=shard-forwarding",
		"firewall-cmd --permanent --policy=shard-forwarding --set-description=managed-by-shard",
		"firewall-cmd --permanent --info-policy=shard-forwarding",
		"firewall-cmd --permanent --policy=shard-forwarding --set-target=ACCEPT",
		"firewall-cmd --permanent --policy=shard-forwarding --add-ingress-zone=shard",
		"firewall-cmd --permanent --policy=shard-forwarding --add-egress-zone=ANY",
		"firewall-cmd --get-zones",
		"firewall-cmd --reload",
	)
}

// A zone and policy already in place, and live, cost no change and no reload.
func TestEnsureZoneLeavesWhatIsInPlace(t *testing.T) {
	f, m := firewalldHost(withMarks(map[string]answer{
		"firewall-cmd --version":                                  {out: "1.3.4\n"},
		"firewall-cmd --permanent --get-zones":                    {out: "public shard trusted\n"},
		"firewall-cmd --permanent --info-zone=shard":              {out: ourZone},
		"firewall-cmd --permanent --get-policies":                 {out: "allow-host-ipv6 shard-forwarding\n"},
		"firewall-cmd --permanent --info-policy=shard-forwarding": {out: ourPol},
		"firewall-cmd --get-zones":                                {out: "public shard trusted\n"},
	}))

	if err := m.EnsureZone(t.Context(), hole); err != nil {
		t.Fatalf("EnsureZone: %v", err)
	}
	requireCalls(t, f,
		"firewall-cmd --state",
		"firewall-cmd --permanent --get-zones",
		"firewall-cmd --permanent --zone=shard --get-description",
		"firewall-cmd --permanent --info-zone=shard",
		"firewall-cmd --version",
		"firewall-cmd --permanent --get-policies",
		"firewall-cmd --permanent --policy=shard-forwarding --get-description",
		"firewall-cmd --permanent --info-policy=shard-forwarding",
		"firewall-cmd --get-zones",
	)
}

// A permanent zone the running firewalld has not loaded yet takes a reload, though nothing changed.
func TestEnsureZoneReloadsAZoneNotYetLive(t *testing.T) {
	f, m := firewalldHost(withMarks(map[string]answer{
		"firewall-cmd --version":                                  {out: "1.3.4\n"},
		"firewall-cmd --permanent --get-zones":                    {out: "public shard\n"},
		"firewall-cmd --permanent --info-zone=shard":              {out: ourZone},
		"firewall-cmd --permanent --get-policies":                 {out: "shard-forwarding\n"},
		"firewall-cmd --permanent --info-policy=shard-forwarding": {out: ourPol},
		"firewall-cmd --get-zones":                                {out: "public\n"},
	}))

	if err := m.EnsureZone(t.Context(), hole); err != nil {
		t.Fatalf("EnsureZone: %v", err)
	}
	if !slices.Contains(f.calls, "firewall-cmd --reload") {
		t.Errorf("calls %q, want a reload", f.calls)
	}
}

// Before 1.0 a zone's target still covers what it forwards, so no policy is made.
func TestEnsureZoneMakesNoPolicyBeforeFirewalldOne(t *testing.T) {
	f, m := firewalldHost(map[string]answer{
		"firewall-cmd --version":                     {out: "0.8.2\n"},
		"firewall-cmd --permanent --get-zones":       {out: "public\n"},
		"firewall-cmd --permanent --info-zone=shard": {out: newZone},
		"firewall-cmd --get-zones":                   {out: "public\n"},
	})

	if err := m.EnsureZone(t.Context(), hole); err != nil {
		t.Fatalf("EnsureZone: %v", err)
	}
	if slices.ContainsFunc(f.calls, func(call string) bool { return strings.Contains(call, "polic") }) {
		t.Errorf("calls %q, want no policy on firewalld 0.8", f.calls)
	}
	if !slices.Contains(f.calls, "firewall-cmd --reload") {
		t.Errorf("calls %q, want a reload for the new zone", f.calls)
	}
}

// Close takes the policy and the zone out of the permanent config, and reloads only when it took something.
func TestCloseRemovesTheZoneAndThePolicy(t *testing.T) {
	f, m := firewalldHost(withMarks(map[string]answer{
		"firewall-cmd --version":                  {out: "1.3.4\n"},
		"firewall-cmd --permanent --get-policies": {out: "allow-host-ipv6 shard-forwarding\n"},
		"firewall-cmd --permanent --get-zones":    {out: "public shard\n"},
	}))

	if err := m.Close(t.Context(), hole); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requireCalls(t, f,
		"firewall-cmd --state",
		"firewall-cmd --version",
		"firewall-cmd --permanent --get-policies",
		"firewall-cmd --permanent --policy=shard-forwarding --get-description",
		"firewall-cmd --permanent --get-zones",
		"firewall-cmd --permanent --zone=shard --get-description",
		"firewall-cmd --permanent --delete-policy=shard-forwarding",
		"firewall-cmd --permanent --delete-zone=shard",
		"firewall-cmd --reload",
	)

	f, m = firewalldHost(map[string]answer{
		"firewall-cmd --version":                  {out: "1.3.4\n"},
		"firewall-cmd --permanent --get-policies": {out: "allow-host-ipv6\n"},
		"firewall-cmd --permanent --get-zones":    {out: "public\n"},
	})
	if err := m.Close(t.Context(), hole); err != nil {
		t.Fatalf("Close a second time: %v", err)
	}
	if slices.Contains(f.calls, "firewall-cmd --reload") {
		t.Errorf("calls %q, want no reload when nothing was there", f.calls)
	}
}

// A zone or policy of that name without the marker is the host's own: EnsureZone refuses it and changes nothing.
func TestEnsureZoneRefusesAZoneOrPolicyItDidNotMake(t *testing.T) {
	cases := map[string]string{
		"zone":   "firewall-cmd --permanent --zone=shard --get-description",
		"policy": "firewall-cmd --permanent --policy=shard-forwarding --get-description",
	}
	for kind, foreign := range cases {
		t.Run(kind, func(t *testing.T) {
			answers := withMarks(map[string]answer{
				"firewall-cmd --version":                                  {out: "1.3.4\n"},
				"firewall-cmd --permanent --get-zones":                    {out: "public shard\n"},
				"firewall-cmd --permanent --info-zone=shard":              {out: ourZone},
				"firewall-cmd --permanent --get-policies":                 {out: "shard-forwarding\n"},
				"firewall-cmd --permanent --info-policy=shard-forwarding": {out: ourPol},
				"firewall-cmd --get-zones":                                {out: "public\n"},
			})
			answers[foreign] = answer{out: "the admin's own\n"}
			f, m := firewalldHost(answers)

			err := m.EnsureZone(t.Context(), hole)
			if err == nil || !strings.Contains(err.Error(), "the host's own") {
				t.Fatalf("EnsureZone = %v, want a refusal of the %s", err, kind)
			}
			if slices.ContainsFunc(f.calls, func(call string) bool {
				return strings.Contains(call, "--set-") || strings.Contains(call, "--add-") || strings.Contains(call, "--reload")
			}) {
				t.Errorf("calls %q, want no change to the host's own %s", f.calls, kind)
			}
		})
	}
}

// Close leaves a zone and a policy of those names without the marker, and reloads nothing.
func TestCloseLeavesAZoneAndPolicyItDidNotMake(t *testing.T) {
	f, m := firewalldHost(map[string]answer{
		"firewall-cmd --version":                                               {out: "1.3.4\n"},
		"firewall-cmd --permanent --get-policies":                              {out: "shard-forwarding\n"},
		"firewall-cmd --permanent --get-zones":                                 {out: "public shard\n"},
		"firewall-cmd --permanent --zone=shard --get-description":              {out: "the admin's own\n"},
		"firewall-cmd --permanent --policy=shard-forwarding --get-description": {out: "\n"},
	})

	if err := m.Close(t.Context(), hole); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if slices.ContainsFunc(f.calls, func(call string) bool {
		return strings.Contains(call, "--delete-") || strings.Contains(call, "--reload")
	}) {
		t.Errorf("calls %q, want the host's own zone and policy left as they are", f.calls)
	}
}
