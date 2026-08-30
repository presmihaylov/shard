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
	"github.com/presmihaylov/shard/services/secret"
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

	web := models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionAllow, "domain:api.example.com")}}
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
		{Name: "web", Rules: []models.Rule{{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomainSuffix, Value: "example.com"}, Protocol: "udp"}}},
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

func TestParseRuleReadsTheCommandLineSpelling(t *testing.T) {
	for _, tc := range []struct {
		text  string
		proto string
		ports []int
		value string
	}{
		{"domain:api.example.com", "tcp", []int{80, 443}, "api.example.com"},
		{"domain:API.example.com. tcp:443", "tcp", []int{443}, "API.example.com."},
		{"domain:api.example.com tcp", "tcp", []int{80, 443}, "api.example.com"},
		{"cidr:10.0.0.0/8 tcp:22,8000-8002", "tcp", []int{22, 8000, 8001, 8002}, "10.0.0.0/8"},
		{"cidr:1.1.1.1 udp:53", "udp", []int{53}, "1.1.1.1"},
		{"group:any udp", "udp", nil, "any"},
		{"group:private", "", nil, "private"},
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
		"", "api.example.com", "domain:", "domain:api.example.com udp", "domain:api.example.com tcp:22",
		"domain-suffix:example.com tcp:22", "cidr:10.0.0.0/8 tcp:9-8", "cidr:10.0.0.0/8 tcp:x", "cidr:10.0.0.0/8 tcp:1-2000",
		"group:any tcp:80 extra", "cidr:2001:db8::/32", "domain:10.0.0.1", "dns:example.com",
	} {
		if _, err := ParseRule(models.ActionAllow, text); err == nil {
			t.Errorf("ParseRule(%q) accepted", text)
		}
	}

	suffix, err := ParseRule(models.ActionAllow, "domain-suffix:example.com")
	if err != nil || suffix.Protocol != "tcp" || !slices.Equal(suffix.Ports, []int{80, 443}) {
		t.Errorf("a domain-suffix rule got %+v, %v, want tcp 80,443", suffix, err)
	}
}

type fakeRecords []models.Sandbox

func (f fakeRecords) List() ([]models.Sandbox, error) { return f, nil }

type fakeGrants map[string][]string

func (f fakeGrants) Get(name string) (secret.Secret, error) {
	dests, ok := f[name]
	if !ok {
		return secret.Secret{}, secret.ErrNotFound
	}

	return secret.Secret{Name: name, Destinations: dests}, nil
}

type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}

	return addrs, nil
}

var nameservers = []netip.Addr{netip.MustParseAddr("1.1.1.1")}

func TestEffectivePutsWhatTheGrantsImplyFirst(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionDeny, "group:any")}}); err != nil {
		t.Fatal(err)
	}

	svc := New(s, nil, fakeGrants{"TOKEN": {"api.example.com"}}, nameservers, fakeResolver{})

	got, err := svc.Effective(models.Sandbox{ID: "sandbox1", Policy: "web", Secrets: []string{"TOKEN", "GONE"}})
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
		"allow domain:api.example.com tcp secret TOKEN",
		"deny group:any  ",
	}
	if !slices.Equal(shape, want) {
		t.Errorf("Effective = %v, want %v", shape, want)
	}

	if got, err := svc.Effective(models.Sandbox{ID: "sandbox2"}); err != nil || got.Policy != "" || got.Rules != nil {
		t.Errorf("a sandbox with no policy got %+v, %v", got, err)
	}
	if got, err := svc.Effective(models.Sandbox{ID: "sandbox3", Policy: "gone"}); err != nil || !got.Missing {
		t.Errorf("a sandbox whose policy is gone got %+v, %v", got, err)
	}
}

func TestChainsResolveOnTheHostAndSkipWhatHasNoAddress(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionAllow, "domain:api.example.com tcp:443")}}); err != nil {
		t.Fatal(err)
	}

	records := fakeRecords{
		{ID: "sandbox1", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")},
		{ID: "sandbox2", Policy: "web"},
		{ID: "sandbox3", Address: netip.MustParsePrefix("10.87.0.3/16")},
	}
	resolver := fakeResolver{"api.example.com": {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("::1"), netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("23.1.1.1")}}

	chains, err := New(s, records, fakeGrants{}, nameservers, resolver).Chains(t.Context())
	if err != nil {
		t.Fatalf("Chains: %v", err)
	}
	if len(chains) != 1 || chains[0].Address != netip.MustParseAddr("10.87.0.2") {
		t.Fatalf("Chains = %+v, want one for sandbox1", chains)
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

func TestChainsFailWhenANameDoesNotResolve(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionAllow, "domain:api.example.com")}}); err != nil {
		t.Fatal(err)
	}

	records := fakeRecords{{ID: "sandbox1", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")}}

	_, err := New(s, records, fakeGrants{}, nameservers, fakeResolver{}).Chains(t.Context())
	if err == nil || !strings.Contains(err.Error(), "sandbox1") || !strings.Contains(err.Error(), "api.example.com") {
		t.Errorf("Chains = %v, want the sandbox and the name", err)
	}
}

func TestChainsCloseAGrantedHostThatDoesNotResolve(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "locked", Rules: []models.Rule{mustRule(t, models.ActionDeny, "group:any")}}); err != nil {
		t.Fatal(err)
	}

	records := fakeRecords{{ID: "sandbox1", Policy: "locked", Secrets: []string{"TOKEN"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}

	chains, err := New(s, records, fakeGrants{"TOKEN": {"gone.example.com"}}, nameservers, fakeResolver{}).Chains(t.Context())
	if err != nil {
		t.Fatalf("Chains: %v", err)
	}
	if len(chains) != 1 || len(chains[0].Rules) != 4 || chains[0].Rules[2].Prefixes != nil {
		t.Errorf("Chains = %+v, want the grant compiled to no address", chains)
	}
}
