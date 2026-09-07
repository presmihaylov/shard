package egress

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

func newStore(t *testing.T) *Store {
	t.Helper()

	s, err := NewStore(filepath.Join(t.TempDir(), "policies"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	return s
}

func mustRule(t *testing.T, action models.Action, text string) models.Rule {
	t.Helper()

	rule, err := ParseRule(action, text)
	if err != nil {
		t.Fatalf("ParseRule(%q): %v", text, err)
	}

	return rule
}

func TestTheStoreRoundTripsAndListsByName(t *testing.T) {
	s := newStore(t)

	web := models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionAllow, "api.example.com")}}
	if err := s.Set(web); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set(models.Policy{Name: "deny-all"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Junk in the directory is not a policy.
	if err := os.WriteFile(filepath.Join(s.dir, ".web.json.tmp"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "web" || len(got.Rules) != 1 || got.Rules[0].Destination.Value != "api.example.com" || !slices.Equal(got.Rules[0].Ports, []int{80, 443}) {
		t.Errorf("Get = %+v", got)
	}

	// A file edited by hand is read through the same gate as Set, so Ensure never renders a rule nft refuses.
	if err := os.WriteFile(filepath.Join(s.dir, "bad.json"), []byte(`{"name":"bad","rules":[{"action":"allow","destination":{"kind":"cidr","value":"1.1.1.1"},"ports":[22]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("bad"); err == nil {
		t.Error("Get accepted a hand-edited policy with ports and no protocol")
	}
	if err := os.Remove(filepath.Join(s.dir, "bad.json")); err != nil {
		t.Fatal(err)
	}

	all, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 || all[0].Name != "deny-all" || all[1].Name != "web" {
		t.Errorf("List = %+v", all)
	}

	if err := s.Remove("web"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := s.Remove("web"); err != nil {
		t.Errorf("a second Remove = %v, want nil", err)
	}
	if _, err := s.Get("web"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Remove = %v, want ErrNotFound", err)
	}
}

func TestTheStoreRefusesWhatTheHostCannotEnforce(t *testing.T) {
	s := newStore(t)

	for _, policy := range []models.Policy{
		{Name: "Web"},
		{Name: "../web"},
		{Name: "web", Rules: []models.Rule{{Action: "permit", Destination: models.Destination{Kind: models.DestinationGroup, Value: "any"}}}},
		{Name: "web", Rules: []models.Rule{{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomainSuffix, Value: "example.com"}, Protocol: "tcp"}}},
		{Name: "web", Rules: []models.Rule{{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "api.example.com"}, Protocol: "udp"}}},
		{Name: "web", Rules: []models.Rule{{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "api.example.com"}, Protocol: "tcp", Ports: []int{22}}}},
		{Name: "web", Rules: []models.Rule{{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationCIDR, Value: "::/0"}}}},
		{Name: "web", Rules: []models.Rule{{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationGroup, Value: "lan"}}}},
		{Name: "web", Rules: []models.Rule{{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationGroup, Value: "any"}, Ports: []int{80}}}},
		{Name: "web", Rules: []models.Rule{{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationGroup, Value: "any"}, Protocol: "tcp", Ports: []int{70000}}}},
	} {
		if err := s.Set(policy); err == nil {
			t.Errorf("Set(%+v) accepted", policy)
		}
	}

	if _, err := os.Stat(filepath.Join(s.dir, "web.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused policy was written: %v", err)
	}
}

func TestValidateRefusesADomainRuleWithNoPort(t *testing.T) {
	policy := models.Policy{Name: "web", Rules: []models.Rule{{
		Action:      models.ActionAllow,
		Destination: models.Destination{Kind: models.DestinationDomain, Value: "api.example.com"},
		Protocol:    "tcp",
	}}}

	err := Validate(policy)
	if err == nil || !strings.Contains(err.Error(), "every tcp port") {
		t.Errorf("Validate of a domain rule with no port = %v", err)
	}
}

func TestParseDestinationReadsTheKindFromTheShape(t *testing.T) {
	for _, tc := range []struct {
		text string
		kind models.DestinationKind
	}{
		{"any", models.DestinationGroup},
		{"dns", models.DestinationGroup},
		{"1.1.1.1", models.DestinationCIDR},
		{"10.0.0.0/8", models.DestinationCIDR},
		{"suffix:example.com", models.DestinationDomainSuffix},
		{"api.example.com", models.DestinationDomain},
	} {
		dest, err := parseDestination(tc.text)
		if err != nil || dest.Kind != tc.kind {
			t.Errorf("parseDestination(%q) = %+v, %v, want kind %s", tc.text, dest, err, tc.kind)
		}
	}
}

func TestParseRuleReadsTheCommandLineSpelling(t *testing.T) {
	for _, tc := range []struct {
		text  string
		proto string
		ports []int
		value string
	}{
		{"api.example.com", "tcp", []int{80, 443}, "api.example.com"},
		{"API.example.com. tcp:443", "tcp", []int{443}, "API.example.com."},
		{"api.example.com tcp", "tcp", []int{80, 443}, "api.example.com"},
		{"10.0.0.0/8 tcp:22,8000-8002", "tcp", []int{22, 8000, 8001, 8002}, "10.0.0.0/8"},
		{"1.1.1.1 udp:53", "udp", []int{53}, "1.1.1.1"},
		{"any udp", "udp", nil, "any"},
	} {
		rule, err := ParseRule(models.ActionDeny, tc.text)
		if err != nil {
			t.Errorf("ParseRule(%q): %v", tc.text, err)

			continue
		}
		if rule.Action != models.ActionDeny || rule.Protocol != tc.proto || !slices.Equal(rule.Ports, tc.ports) || rule.Destination.Value != tc.value {
			t.Errorf("ParseRule(%q) = %+v", tc.text, rule)
		}
	}

	for _, text := range []string{
		"", "api.example.com udp", "api.example.com tcp:22",
		"suffix:example.com tcp:22", "10.0.0.0/8 tcp:9-8", "10.0.0.0/8 tcp:x", "10.0.0.0/8 tcp:1-2000",
		"any tcp:80 extra", "2001:db8::/32", "dns:example.com",
		"private", "group:private",
		"domain:api.example.com", "cidr:1.1.1.1", "group:any", "domain-suffix:example.com",
	} {
		if _, err := ParseRule(models.ActionAllow, text); err == nil {
			t.Errorf("ParseRule(%q) accepted", text)
		}
	}

	rule, err := ParseRule(models.ActionAllow, "suffix:example.com")
	if err != nil || rule.Protocol != "tcp" || !slices.Equal(rule.Ports, []int{80, 443}) {
		t.Errorf("a suffix rule = %+v, %v, want the web ports by default", rule, err)
	}
}

func TestMatchHostReadsTheWildcardShape(t *testing.T) {
	for _, tc := range []struct {
		pattern, host string
		want          bool
	}{
		{"*", "anything.example.com", true},
		{"api.example.com", "api.example.com", true},
		{"api.example.com", "www.example.com", false},
		{"*.example.com", "api.example.com", true},
		{"*.example.com", "deep.api.example.com", true},
		{"*.example.com", "example.com", false},
		{"*.example.com", "example.org", false},
		{"www.*.com", "www.example.com", true},
		{"www.*.com", "www.deep.example.com", false},
		{"*.*.example.com", "a.b.example.com", true},
		{"*.*.example.com", "b.example.com", false},
	} {
		if got := MatchHost(tc.pattern, tc.host); got != tc.want {
			t.Errorf("MatchHost(%q, %q) = %v, want %v", tc.pattern, tc.host, got, tc.want)
		}
	}
}

func TestDecideWalksTheEffectiveRulesByName(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{
		mustRule(t, models.ActionDeny, "bad.example.com"),
		mustRule(t, models.ActionAllow, "suffix:example.com"),
		mustRule(t, models.ActionAllow, "*.example.org tcp:443"),
		mustRule(t, models.ActionAllow, "93.184.216.0/24 tcp:80"),
	}}); err != nil {
		t.Fatal(err)
	}

	svc := New(s, nil, nameservers, fakeResolver{})
	sb := models.Sandbox{ID: "sandbox1", Policy: "web", Secrets: []string{"TOKEN"}}
	public := netip.MustParseAddr("93.184.216.34")

	for _, tc := range []struct {
		host string
		port int
		addr netip.Addr
		want models.Action
		rule string
	}{
		{"bad.example.com", 443, public, models.ActionDeny, "deny bad.example.com tcp:80,443"},
		{"api.example.com", 80, public, models.ActionAllow, "allow suffix:example.com tcp:80,443"},
		{"example.com", 80, public, models.ActionAllow, "allow suffix:example.com tcp:80,443"},
		{"notexample.com", 443, public, models.ActionDeny, ""},
		{"a.example.org", 443, public, models.ActionAllow, "allow *.example.org tcp:443"},
		{"a.example.org", 80, netip.MustParseAddr("93.184.216.9"), models.ActionAllow, "allow 93.184.216.0/24 tcp:80"},
		{"a.example.org", 80, netip.MustParseAddr("1.2.3.4"), models.ActionDeny, ""},
		{"api.example.com", 443, netip.MustParseAddr("10.0.0.5"), models.ActionDeny, ""},
	} {
		got, err := svc.Decide(sb, tc.host, tc.port, tc.addr)
		if err != nil {
			t.Fatalf("Decide(%s:%d): %v", tc.host, tc.port, err)
		}
		rule := ""
		if got.Rule.Destination.Kind != "" {
			rule = FormatRule(got.Rule.Rule)
		}
		if got.Action != tc.want || rule != tc.rule {
			t.Errorf("Decide(%s:%d at %s) = %s by %q (%s), want %s by %q", tc.host, tc.port, tc.addr, got.Action, rule, got.Reason, tc.want, tc.rule)
		}
	}

	if got, err := svc.Decide(models.Sandbox{ID: "free"}, "any.example.net", 80, public); err != nil || got.Action != models.ActionAllow {
		t.Errorf("a sandbox with no policy got %+v, %v", got, err)
	}
	if got, err := svc.Decide(models.Sandbox{ID: "lost", Policy: "gone"}, "any.example.net", 80, public); err != nil || got.Action != models.ActionDeny {
		t.Errorf("a sandbox whose policy is gone got %+v, %v", got, err)
	}
}

type fakeRecords []models.Sandbox

func (f fakeRecords) List() ([]models.Sandbox, error) { return f, nil }

type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}

	return addrs, nil
}

var nameservers = []netip.Addr{netip.MustParseAddr("1.1.1.1")}

func TestEffectiveIsThePolicysRulesAndTheDNSTheyNeed(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{
		mustRule(t, models.ActionAllow, "api.example.com tcp:443"),
		mustRule(t, models.ActionAllow, "93.184.216.0/24 tcp:80"),
		mustRule(t, models.ActionDeny, "any"),
	}}); err != nil {
		t.Fatal(err)
	}

	svc := New(s, nil, nameservers, fakeResolver{})
	sb := models.Sandbox{ID: "sandbox1", Policy: "web", Secrets: []string{"TOKEN", "GONE"}}

	got, err := svc.Effective(sb)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}

	var shape []string
	for _, rule := range got.Rules {
		shape = append(shape, string(rule.Action)+" "+string(rule.Destination.Kind)+":"+rule.Destination.Value+" "+rule.Protocol+" "+rule.Implied)
	}
	want := []string{
		"allow cidr:1.1.1.1 udp dns",
		"allow cidr:1.1.1.1 tcp dns",
		"allow domain:api.example.com tcp ",
		"allow cidr:93.184.216.0/24 tcp ",
		"deny group:any  ",
	}
	if !slices.Equal(shape, want) {
		t.Errorf("Effective = %v, want %v", shape, want)
	}

	// The proxy logs the id and the host chains log the same one, so the ids count the effective order.
	var ids []string
	for _, rule := range got.Rules {
		ids = append(ids, rule.ID)
	}
	if !slices.Equal(ids, []string{"1", "2", "3", "4", "5"}) {
		t.Errorf("Effective gave the ids %v", ids)
	}

	svc = New(newStore(t), nil, nameservers, fakeResolver{})
	if got, err := svc.Effective(models.Sandbox{ID: "sandbox2"}); err != nil || got.Policy != "" || got.Rules != nil {
		t.Errorf("a sandbox with no policy got %+v, %v", got, err)
	}
	if got, err := svc.Effective(models.Sandbox{ID: "sandbox3", Policy: "gone"}); err != nil || !got.Missing {
		t.Errorf("a sandbox whose policy is gone got %+v, %v", got, err)
	}
}

// A policy of addresses only opens no DNS, and a secret does not open it either.
func TestEffectiveOpensNoDNSForAPolicyThatNamesNothing(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "addresses", Rules: []models.Rule{
		mustRule(t, models.ActionAllow, "93.184.216.0/24 tcp:80"),
	}}); err != nil {
		t.Fatal(err)
	}

	got, err := New(s, nil, nameservers, fakeResolver{}).Effective(models.Sandbox{ID: "sandbox1", Policy: "addresses", Secrets: []string{"TOKEN"}})
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if len(got.Rules) != 1 || got.Rules[0].Implied != "" {
		t.Errorf("Effective = %+v, want the one address rule and no dns", got.Rules)
	}
}

// A grant says where a value may go, never what the sandbox may reach: the policy alone decides that.
func TestDecideDeniesAGrantedHostThePolicyDoesNotAllow(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "locked", Rules: []models.Rule{
		mustRule(t, models.ActionAllow, "hooks.example.net tcp:443"),
		mustRule(t, models.ActionDeny, "any"),
	}}); err != nil {
		t.Fatal(err)
	}

	svc := New(s, nil, nameservers, fakeResolver{})
	sb := models.Sandbox{ID: "sandbox1", Policy: "locked", Secrets: []string{"TOKEN"}}
	public := netip.MustParseAddr("93.184.216.34")

	got, err := svc.Decide(sb, "api.example.com", 443, public)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Action != models.ActionDeny || got.Rule.ID != "4" {
		t.Errorf("the granted host got %s by rule %q, want a deny by the catch-all", got.Action, got.Rule.ID)
	}
	if decision, err := svc.Decide(sb, "hooks.example.net", 443, public); err != nil || decision.Action != models.ActionAllow {
		t.Errorf("the host the policy allows got %+v, %v", decision, err)
	}
}

func TestChainsResolveOnTheHostAndSkipWhatHasNoAddress(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionAllow, "api.example.com tcp:443")}}); err != nil {
		t.Fatal(err)
	}

	records := fakeRecords{
		{ID: "sandbox1", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")},
		{ID: "sandbox2", Policy: "web"},
		{ID: "sandbox3", Address: netip.MustParsePrefix("10.87.0.3/16")},
		{ID: "sandbox4", Secrets: []string{"TOKEN"}, Address: netip.MustParsePrefix("10.87.0.4/16")},
	}
	resolver := fakeResolver{"api.example.com": {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("::1"), netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("23.1.1.1")}}

	chains, err := New(s, records, nameservers, resolver).Chains(t.Context())
	if err != nil {
		t.Fatalf("Chains: %v", err)
	}
	if len(chains) != 2 || chains[0].Address != netip.MustParseAddr("10.87.0.2") || !chains[0].Policy {
		t.Fatalf("Chains = %+v, want one for sandbox1 and one for sandbox4", chains)
	}
	// A secret alone fronts the sandbox, so it gets the proxy and no rules of its own.
	if chains[1].Address != netip.MustParseAddr("10.87.0.4") || chains[1].Policy || chains[1].Rules != nil {
		t.Errorf("the secret-only sandbox compiled to %+v", chains[1])
	}

	rules := chains[0].Rules
	if len(rules) != 3 {
		t.Fatalf("rules = %+v, want dns udp, dns tcp and the domain", rules)
	}
	if got := rules[2].Prefixes; len(got) != 2 || got[0].String() != "23.1.1.1/32" || got[1].String() != "93.184.216.34/32" {
		t.Errorf("the domain compiled to %v, want its IPv4 addresses once each, sorted", got)
	}
	if rules[0].Protocol != "udp" || !slices.Equal(rules[0].Ports, []int{53}) || rules[0].Prefixes[0].String() != "1.1.1.1/32" {
		t.Errorf("dns compiled to %+v", rules[0])
	}
}

// A stopped sandbox keeps its lease, so it keeps its chain: a rewrite while it is down must not drop it (SHARD-118).
func TestChainsKeepAStoppedSandboxesChain(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionDeny, "any")}}); err != nil {
		t.Fatal(err)
	}

	records := fakeRecords{{ID: "sandbox1", Policy: "web", State: models.StateStopped, Address: netip.MustParsePrefix("10.87.0.2/16")}}

	chains, err := New(s, records, nameservers, fakeResolver{}).Chains(t.Context())
	if err != nil {
		t.Fatalf("Chains: %v", err)
	}
	if len(chains) != 1 || chains[0].Address != netip.MustParseAddr("10.87.0.2") {
		t.Fatalf("Chains = %+v, want one for the stopped sandbox1", chains)
	}
}

func TestChainsFailWhenANameDoesNotResolve(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionAllow, "api.example.com")}}); err != nil {
		t.Fatal(err)
	}

	records := fakeRecords{{ID: "sandbox1", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")}}

	_, err := New(s, records, nameservers, fakeResolver{}).Chains(t.Context())
	if err == nil || !strings.Contains(err.Error(), "sandbox1") || !strings.Contains(err.Error(), "api.example.com") {
		t.Errorf("Chains = %v, want the sandbox and the name", err)
	}
}

func TestValidateWildcardDomainRules(t *testing.T) {
	good := []string{"*", "*.example.com", "www.*.com"}
	for _, text := range good {
		if _, err := ParseRule(models.ActionAllow, text); err != nil {
			t.Errorf("ParseRule refused %q: %v", text, err)
		}
	}

	bad := []string{"api*.example.com", "suffix:*.example.com", "suffix:*"}
	for _, text := range bad {
		_, err := ParseRule(models.ActionAllow, text)
		if err == nil {
			t.Errorf("ParseRule accepted %q", text)

			continue
		}
		// A mistyped rule may carry a secret value, so no refusal echoes the host it refused.
		if strings.Contains(err.Error(), strings.TrimPrefix(text, "suffix:")) {
			t.Errorf("the refusal echoes the host it refused: %v", err)
		}
	}
}

func TestValidateNamesThePositionOfTheRuleItRefused(t *testing.T) {
	policy := models.Policy{Name: "web", Rules: []models.Rule{
		{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "api.example.com"}, Protocol: "tcp", Ports: []int{443}},
		{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "sk_live_secret"}, Protocol: "tcp", Ports: []int{443}},
	}}

	err := Validate(policy)
	if err == nil || !strings.Contains(err.Error(), "rule 2") {
		t.Errorf("Validate = %v, want the position of the rule it refused", err)
	}
	if err != nil && strings.Contains(err.Error(), "sk_live_secret") {
		t.Errorf("the refusal echoes the host it refused: %v", err)
	}
}

// dns is a destination the operator can say outright, and it is closed until an allow opens it.
func TestAllowDNSIsARuleAndDenyDNSIsRefused(t *testing.T) {
	rule, err := ParseRule(models.ActionAllow, "dns")
	if err != nil {
		t.Fatalf("ParseRule(allow dns): %v", err)
	}
	if rule.Destination.Kind != models.DestinationGroup || rule.Destination.Value != "dns" {
		t.Errorf("allow dns parsed as %+v", rule.Destination)
	}
	if err := Validate(models.Policy{Name: "web", Rules: []models.Rule{rule}}); err != nil {
		t.Errorf("Validate of an allow dns policy: %v", err)
	}

	_, err = ParseRule(models.ActionDeny, "dns")
	if err == nil || !strings.Contains(err.Error(), "dns is closed unless a rule or a name rule opens it") {
		t.Errorf("ParseRule(deny dns) = %v", err)
	}
}

// The explicit rule opens the same door a name rule does, and the implied rule says which one asked.
func TestEffectiveOpensDNSForAnAllowDNSRuleAlone(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "addr", Rules: []models.Rule{
		mustRule(t, models.ActionAllow, "203.0.113.7 tcp:443"),
		mustRule(t, models.ActionAllow, "dns"),
		mustRule(t, models.ActionDeny, "any"),
	}}); err != nil {
		t.Fatal(err)
	}

	got, err := New(s, nil, nameservers, fakeResolver{}).Effective(models.Sandbox{ID: "sandbox1", Policy: "addr"})
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}

	var shape []string
	for _, rule := range got.Rules {
		shape = append(shape, string(rule.Action)+" "+string(rule.Destination.Kind)+":"+rule.Destination.Value+" "+rule.Protocol+" "+rule.Implied)
	}
	want := []string{
		"allow cidr:1.1.1.1 udp dns rule",
		"allow cidr:1.1.1.1 tcp dns rule",
		"allow cidr:203.0.113.7 tcp ",
		"allow group:dns  ",
		"deny group:any  ",
	}
	if !slices.Equal(shape, want) {
		t.Errorf("Effective = %v, want %v", shape, want)
	}
}

func TestOpensDNSReadsWhatEachPolicyAsksFor(t *testing.T) {
	for _, tc := range []struct {
		rule string
		want bool
	}{
		{"api.example.com", true},
		{"suffix:example.com", true},
		{"dns", true},
		{"203.0.113.7 tcp:443", false},
		{"any", false},
	} {
		policy := models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionAllow, tc.rule)}}
		if got := OpensDNS(policy); got != tc.want {
			t.Errorf("OpensDNS(allow %s) = %v, want %v", tc.rule, got, tc.want)
		}
	}
}
