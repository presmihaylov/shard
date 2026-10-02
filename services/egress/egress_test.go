package egress

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/network"
)

func newStore(t *testing.T) *Store {
	t.Helper()

	s, err := NewStore(filepath.Join(t.TempDir(), "policies"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	return s
}

// SHARD-167: a list page walks the names in byte order, so a name that is a prefix of another comes first.
func TestListSortsByNameNotByFileName(t *testing.T) {
	s := newStore(t)

	for _, name := range []string{"deny-all", "deny"} {
		if err := s.Set(models.Policy{Name: name}); err != nil {
			t.Fatalf("Set %s: %v", name, err)
		}
	}

	all, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 || all[0].Name != "deny" || all[1].Name != "deny-all" {
		t.Errorf("List = %+v, want deny before deny-all", all)
	}
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
		{"API.example.com. tcp:443", "tcp", []int{443}, "api.example.com"},
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

// A deny typed in any case or with a trailing dot matches the host the proxy and the resolver compare, parsed now or stored as typed before (SHARD-303).
func TestADenyMatchesHoweverItsHostIsTyped(t *testing.T) {
	public := netip.MustParseAddr("93.184.216.34")
	for _, tc := range []struct {
		kind  models.DestinationKind
		typed string
		host  string
	}{
		{models.DestinationDomain, "ExAmPlE.com", "example.com"},
		{models.DestinationDomain, "example.com.", "example.com"},
		{models.DestinationDomain, "*.ExAmPlE.com.", "api.example.com"},
		{models.DestinationDomainSuffix, "ExAmPlE.com.", "api.example.com"},
	} {
		text := tc.typed
		if tc.kind == models.DestinationDomainSuffix {
			text = "suffix:" + tc.typed
		}
		typed := models.Rule{Action: models.ActionDeny, Destination: models.Destination{Kind: tc.kind, Value: tc.typed}, Protocol: "tcp", Ports: []int{80, 443}}

		for _, deny := range []models.Rule{mustRule(t, models.ActionDeny, text), typed} {
			s := newStore(t)
			blob, err := json.Marshal(models.Policy{Name: "web", Rules: []models.Rule{deny, mustRule(t, models.ActionAllow, "any")}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.path("web"), blob, 0o600); err != nil {
				t.Fatal(err)
			}

			svc := New(s, nil, gateway, nameservers, fakeResolver{})
			sb := models.Sandbox{ID: "sandbox1", Policy: "web"}
			byAddr, err := svc.Decide(sb, tc.host, 443, public)
			if err != nil {
				t.Fatal(err)
			}
			byName, err := svc.DecideName(sb, tc.host)
			if err != nil {
				t.Fatal(err)
			}
			if byAddr.Action != models.ActionDeny || byName.Action != models.ActionDeny {
				t.Errorf("a deny of %q stored as %q: Decide(%s) = %s, DecideName = %s, want both deny", text, deny.Destination.Value, tc.host, byAddr.Action, byName.Action)
			}
		}
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

	svc := New(s, nil, gateway, nameservers, fakeResolver{})
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

// The proxy dials from the host, so what is local to it and not already private is refused under every policy (SHARD-291).
func TestDecideRefusesWhatIsLocalToTheHost(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "open", Rules: []models.Rule{mustRule(t, models.ActionAllow, "any")}}); err != nil {
		t.Fatal(err)
	}

	svc := New(s, nil, gateway, nameservers, fakeResolver{})
	svc.local.Addresses = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("203.0.113.9")}, nil }

	for _, sb := range []models.Sandbox{{ID: "free"}, {ID: "open", Policy: "open"}} {
		for _, addr := range []string{"0.0.0.0", "0.1.2.3", "203.0.113.9", "224.0.0.251", "255.255.255.255"} {
			got, err := svc.Decide(sb, addr, 80, netip.MustParseAddr(addr))
			if err != nil || got.Action != models.ActionDeny || got.ID != network.RuleLocal {
				t.Errorf("sandbox %s to %s got %+v, %v, want a local deny", sb.ID, addr, got, err)
			}
		}
		if got, err := svc.Decide(sb, "203.0.113.7", 80, netip.MustParseAddr("203.0.113.7")); err != nil || got.Action != models.ActionAllow {
			t.Errorf("sandbox %s to an address the host does not own got %+v, %v", sb.ID, got, err)
		}
	}
}

// The 403 body reaches the guest, so a floor deny names only the host and never the address it resolved to (SHARD-342).
func TestDecideFloorReasonCarriesNoResolvedAddress(t *testing.T) {
	svc := New(newStore(t), nil, gateway, nameservers, fakeResolver{})
	svc.local.Addresses = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("203.0.113.9")}, nil }

	for _, tc := range []struct {
		name string
		addr netip.Addr
		id   string
	}{
		{"private", netip.MustParseAddr("10.0.0.5"), network.RulePrivate},
		{"local", netip.MustParseAddr("203.0.113.9"), network.RuleLocal},
	} {
		got, err := svc.Decide(models.Sandbox{ID: "sb"}, "forged.example.com", 80, tc.addr)
		if err != nil || got.ID != tc.id {
			t.Fatalf("%s floor: got %+v, %v", tc.name, got, err)
		}
		if strings.Contains(got.Reason, tc.addr.String()) {
			t.Errorf("%s floor reason %q carries the resolved address %s (SHARD-342)", tc.name, got.Reason, tc.addr)
		}
	}
}

// A question carries a name and no address, so an address rule is silent and a deny on one port leaves the name in use.
func TestDecideNameJudgesAQuestionByTheNameAlone(t *testing.T) {
	s := newStore(t)
	for _, policy := range []models.Policy{
		{Name: "web", Rules: []models.Rule{
			mustRule(t, models.ActionDeny, "bad.example.com"),
			mustRule(t, models.ActionDeny, "half.example.com tcp:443"),
			mustRule(t, models.ActionAllow, "suffix:example.com"),
			mustRule(t, models.ActionAllow, "*.example.org tcp:443"),
			mustRule(t, models.ActionAllow, "93.184.216.0/24 tcp:80"),
		}},
		{Name: "addresses", Rules: []models.Rule{
			mustRule(t, models.ActionAllow, "93.184.216.0/24"),
			mustRule(t, models.ActionAllow, "any udp:123"),
		}},
		{Name: "open", Rules: []models.Rule{
			mustRule(t, models.ActionDeny, "any tcp:22"),
			mustRule(t, models.ActionAllow, "dns"),
			mustRule(t, models.ActionDeny, "any"),
		}},
		{Name: "shut", Rules: []models.Rule{
			mustRule(t, models.ActionDeny, "any tcp"),
			mustRule(t, models.ActionAllow, "api.example.com"),
		}},
		{Name: "wide", Rules: []models.Rule{mustRule(t, models.ActionAllow, "any")}},
	} {
		if err := s.Set(policy); err != nil {
			t.Fatal(err)
		}
	}

	svc := New(s, nil, gateway, nameservers, fakeResolver{})

	for _, tc := range []struct {
		policy string
		name   string
		want   models.Action
		rule   string
	}{
		{"web", "bad.example.com", models.ActionDeny, "deny bad.example.com tcp:80,443"},
		{"web", "half.example.com", models.ActionAllow, "allow suffix:example.com tcp:80,443"},
		{"web", "example.com", models.ActionAllow, "allow suffix:example.com tcp:80,443"},
		{"web", "notexample.com", models.ActionDeny, ""},
		{"web", "a.example.org", models.ActionAllow, "allow *.example.org tcp:443"},
		{"web", "example.org", models.ActionDeny, ""},
		{"addresses", "api.example.com", models.ActionDeny, ""},
		{"open", "any.example.net", models.ActionAllow, "allow dns"},
		{"shut", "api.example.com", models.ActionDeny, "deny any tcp"},
		{"wide", "any.example.net", models.ActionAllow, "allow any"},
	} {
		got, err := svc.DecideName(models.Sandbox{ID: "sandbox1", Policy: tc.policy}, tc.name)
		if err != nil {
			t.Fatalf("DecideName(%s under %s): %v", tc.name, tc.policy, err)
		}
		rule := ""
		if got.Rule.Destination.Kind != "" {
			rule = FormatRule(got.Rule.Rule)
		}
		if got.Action != tc.want || rule != tc.rule {
			t.Errorf("DecideName(%s under %s) = %s by %q (%s), want %s by %q", tc.name, tc.policy, got.Action, rule, got.Reason, tc.want, tc.rule)
		}
	}

	if got, err := svc.DecideName(models.Sandbox{ID: "free"}, "any.example.net"); err != nil || got.Action != models.ActionAllow || got.ID != network.RuleNone {
		t.Errorf("a sandbox with no policy got %+v, %v", got, err)
	}
	if got, err := svc.DecideName(models.Sandbox{ID: "lost", Policy: "gone"}, "any.example.net"); err != nil || got.Action != models.ActionDeny || got.ID != network.RuleMissing {
		t.Errorf("a sandbox whose policy is gone got %+v, %v", got, err)
	}
}

// The http path judges the name before any lookup: a host no rule allows is refused unresolved, and only an
// allow rule an address could match on the port forces a resolve (SHARD-342).
func TestUnresolvedRefusesByNameBeforeResolving(t *testing.T) {
	s := newStore(t)
	for _, policy := range []models.Policy{
		{Name: "denyall", Rules: []models.Rule{mustRule(t, models.ActionDeny, "any")}},
		{Name: "namelist", Rules: []models.Rule{mustRule(t, models.ActionAllow, "api.example.com")}},
		{Name: "namedeny", Rules: []models.Rule{
			mustRule(t, models.ActionDeny, "bad.example.com"),
			mustRule(t, models.ActionAllow, "suffix:example.com"),
		}},
		{Name: "cidr", Rules: []models.Rule{mustRule(t, models.ActionAllow, "93.184.216.0/24")}},
		{Name: "cidr443", Rules: []models.Rule{mustRule(t, models.ActionAllow, "93.184.216.0/24 tcp:443")}},
		{Name: "denycidr", Rules: []models.Rule{
			mustRule(t, models.ActionDeny, "8.8.8.8/32"),
			mustRule(t, models.ActionAllow, "api.example.com"),
		}},
		{Name: "dnsonly", Rules: []models.Rule{mustRule(t, models.ActionAllow, "dns")}},
		{Name: "web80", Rules: []models.Rule{mustRule(t, models.ActionAllow, "api.example.com tcp:80")}},
		{Name: "denyport", Rules: []models.Rule{
			mustRule(t, models.ActionDeny, "bad.example.com tcp:443"),
			mustRule(t, models.ActionAllow, "93.184.216.0/24"),
		}},
	} {
		if err := s.Set(policy); err != nil {
			t.Fatal(err)
		}
	}

	svc := New(s, nil, gateway, nameservers, fakeResolver{})

	for _, tc := range []struct {
		policy string
		host   string
		port   int
		final  bool
		action models.Action
		rule   string
	}{
		{"denyall", "0a1402.t.attacker.test", 80, true, models.ActionDeny, "deny any"},
		{"namelist", "api.example.com", 443, false, "", ""},
		{"namelist", "evil.test", 443, true, models.ActionDeny, ""},
		{"namedeny", "bad.example.com", 443, true, models.ActionDeny, "deny bad.example.com tcp:80,443"},
		{"namedeny", "sub.example.com", 443, false, "", ""},
		{"cidr", "evil.test", 443, false, "", ""},
		{"cidr443", "evil.test", 80, true, models.ActionDeny, ""},
		{"cidr443", "evil.test", 443, false, "", ""},
		{"denycidr", "evil.test", 443, true, models.ActionDeny, ""},
		// An allow-dns-only policy must refuse a forged Host unresolved: the dns group is the resolver, not a web host.
		{"dnsonly", "exfil.attacker.test", 443, true, models.ActionDeny, ""},
		// A name rule bound to one web port must not open the other: 443 is refused unresolved, 80 goes on to resolve.
		{"web80", "api.example.com", 443, true, models.ActionDeny, ""},
		{"web80", "api.example.com", 80, false, "", ""},
		// A per-port deny is final at its port before a later address allow can resolve the name.
		{"denyport", "bad.example.com", 443, true, models.ActionDeny, "deny bad.example.com tcp:443"},
		{"denyport", "bad.example.com", 80, false, "", ""},
	} {
		got, final, err := svc.Unresolved(models.Sandbox{ID: "sandbox1", Policy: tc.policy}, tc.host, tc.port)
		if err != nil {
			t.Fatalf("Unresolved(%s under %s:%d): %v", tc.host, tc.policy, tc.port, err)
		}
		if final != tc.final {
			t.Errorf("Unresolved(%s under %s:%d) final = %v, want %v", tc.host, tc.policy, tc.port, final, tc.final)
		}
		if !final {
			continue
		}
		rule := ""
		if got.Rule.Destination.Kind != "" {
			rule = FormatRule(got.Rule.Rule)
		}
		if got.Action != tc.action || rule != tc.rule {
			t.Errorf("Unresolved(%s under %s:%d) = %s by %q, want %s by %q", tc.host, tc.policy, tc.port, got.Action, rule, tc.action, tc.rule)
		}
		if strings.Contains(got.Reason, "resolves to") {
			t.Errorf("Unresolved(%s under %s:%d) reason carried an address: %q", tc.host, tc.policy, tc.port, got.Reason)
		}
	}

	if _, final, err := svc.Unresolved(models.Sandbox{ID: "free"}, "any.example.net", 443); err != nil || final {
		t.Errorf("a sandbox with no policy was refused unresolved (final=%v, %v)", final, err)
	}
	if got, final, err := svc.Unresolved(models.Sandbox{ID: "lost", Policy: "gone"}, "any.example.net", 443); err != nil || !final || got.ID != network.RuleMissing {
		t.Errorf("a sandbox whose policy is gone got %+v (final=%v), %v", got, final, err)
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

var (
	gateway     = netip.MustParseAddr("10.87.0.1")
	nameservers = []netip.Addr{netip.MustParseAddr("1.1.1.1")}
)

func TestEffectiveIsThePolicysRulesAndTheDNSTheyNeed(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{
		mustRule(t, models.ActionAllow, "api.example.com tcp:443"),
		mustRule(t, models.ActionAllow, "93.184.216.0/24 tcp:80"),
		mustRule(t, models.ActionDeny, "any"),
	}}); err != nil {
		t.Fatal(err)
	}

	svc := New(s, nil, gateway, nameservers, fakeResolver{})
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
		"allow cidr:10.87.0.1 udp dns",
		"allow cidr:10.87.0.1 tcp dns",
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

	svc = New(newStore(t), nil, gateway, nameservers, fakeResolver{})
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

	got, err := New(s, nil, gateway, nameservers, fakeResolver{}).Effective(models.Sandbox{ID: "sandbox1", Policy: "addresses", Secrets: []string{"TOKEN"}})
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

	svc := New(s, nil, gateway, nameservers, fakeResolver{})
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

	chains, err := New(s, records, gateway, nameservers, resolver).Chains(t.Context())
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
	if rules[0].Protocol != "udp" || !slices.Equal(rules[0].Ports, []int{53}) || rules[0].Prefixes[0].String() != "10.87.0.1/32" {
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

	chains, err := New(s, records, gateway, nameservers, fakeResolver{}).Chains(t.Context())
	if err != nil {
		t.Fatalf("Chains: %v", err)
	}
	if len(chains) != 1 || chains[0].Address != netip.MustParseAddr("10.87.0.2") {
		t.Fatalf("Chains = %+v, want one for the stopped sandbox1", chains)
	}
}

// A name that does not resolve closes its sandbox's chain and holds no other sandbox (SHARD-335).
func TestChainsCloseASandboxWhoseNameDoesNotResolve(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionAllow, "api.example.com")}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(models.Policy{Name: "closed", Rules: []models.Rule{mustRule(t, models.ActionDeny, "any")}}); err != nil {
		t.Fatal(err)
	}

	records := fakeRecords{
		{ID: "sandbox1", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")},
		{ID: "sandbox2", Policy: "closed", Address: netip.MustParsePrefix("10.87.0.3/16")},
	}

	chains, err := New(s, records, gateway, nameservers, fakeResolver{}).Chains(t.Context())
	held := heldOf(t, err)
	if len(held.Errs) != 1 || !strings.Contains(err.Error(), "sandbox1") || !strings.Contains(err.Error(), "api.example.com") || !strings.Contains(err.Error(), "closed chain") {
		t.Errorf("Chains = %v, want sandbox1 on a closed chain, naming the host", err)
	}
	if len(chains) != 2 || !chains[0].Policy || chains[0].Rules != nil {
		t.Fatalf("Chains = %+v, want sandbox1 closed and sandbox2 whole", chains)
	}
	if len(chains[1].Rules) == 0 {
		t.Errorf("sandbox2 compiled to %+v, want its rules", chains[1])
	}
}

// A sandbox whose name stops resolving keeps its last good chain while its policy is unchanged, and gets a closed one once it changes.
func TestChainsHoldTheLastGoodChainOfAnUnchangedPolicy(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionAllow, "api.example.com tcp:443")}}); err != nil {
		t.Fatal(err)
	}
	records := fakeRecords{{ID: "sandbox1", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")}}
	resolver := fakeResolver{"api.example.com": {netip.MustParseAddr("93.184.216.34")}}
	svc := New(s, records, gateway, nameservers, resolver)

	good, err := svc.Chains(t.Context())
	if err != nil || len(good) != 1 || len(good[0].Rules) != 3 {
		t.Fatalf("Chains = %+v, %v", good, err)
	}

	delete(resolver, "api.example.com")
	chains, err := svc.Chains(t.Context())
	if heldOf(t, err); !strings.Contains(err.Error(), "last good chain") || !reflect.DeepEqual(chains, good) {
		t.Errorf("Chains = %+v, %v; want the last good chain", chains, err)
	}

	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionAllow, "api.example.com tcp:80")}}); err != nil {
		t.Fatal(err)
	}
	chains, err = svc.Chains(t.Context())
	if heldOf(t, err); !strings.Contains(err.Error(), "closed chain") || len(chains) != 1 || chains[0].Rules != nil {
		t.Errorf("Chains = %+v, %v; want a closed chain once the policy changed", chains, err)
	}

	resolver["api.example.com"] = []netip.Addr{netip.MustParseAddr("93.184.216.34")}
	if chains, err := svc.Chains(t.Context()); err != nil || len(chains) != 1 || len(chains[0].Rules) != 3 {
		t.Errorf("Chains = %+v, %v; want the sandbox whole once the name resolves", chains, err)
	}
}

// A sandbox that left the records forgets its last good chain, so a later one with the same id never inherits it.
func TestChainsForgetASandboxThatWent(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionAllow, "api.example.com tcp:443")}}); err != nil {
		t.Fatal(err)
	}
	sandbox1 := models.Sandbox{ID: "sandbox1", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")}
	records := fakeRecords{sandbox1}
	resolver := fakeResolver{"api.example.com": {netip.MustParseAddr("93.184.216.34")}}
	svc := New(s, &records, gateway, nameservers, resolver)

	if _, err := svc.Chains(t.Context()); err != nil {
		t.Fatal(err)
	}
	records = nil
	if _, err := svc.Chains(t.Context()); err != nil {
		t.Fatal(err)
	}

	records = fakeRecords{sandbox1}
	delete(resolver, "api.example.com")
	chains, err := svc.Chains(t.Context())
	if heldOf(t, err); !strings.Contains(err.Error(), "closed chain") || len(chains) != 1 || chains[0].Rules != nil {
		t.Errorf("Chains = %+v, %v; want a closed chain, not the forgotten one", chains, err)
	}
}

// A shutdown fails every lookup at once, so it fails the compile whole and holds nobody.
func TestChainsFailWholeOnACancelledContext(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionAllow, "api.example.com")}}); err != nil {
		t.Fatal(err)
	}
	records := fakeRecords{{ID: "sandbox1", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var held *network.HeldChains
	if _, err := New(s, records, gateway, nameservers, fakeResolver{}).Chains(ctx); err == nil || errors.As(err, &held) {
		t.Errorf("Chains = %v, want a whole failure", err)
	}
}

func heldOf(t *testing.T, err error) *network.HeldChains {
	t.Helper()

	var held *network.HeldChains
	if !errors.As(err, &held) {
		t.Fatalf("Chains = %v, want a held compile", err)
	}

	return held
}

// A failed create is terminal, so a name of its policy that no longer resolves must not fail every other apply (SHARD-276).
func TestChainsSkipAFailedSandbox(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "bad", Rules: []models.Rule{mustRule(t, models.ActionAllow, "gone.example.com")}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionDeny, "any")}}); err != nil {
		t.Fatal(err)
	}

	records := fakeRecords{
		{ID: "sandbox1", Policy: "bad", State: models.StateFailed, Address: netip.MustParsePrefix("10.87.0.2/16")},
		{ID: "sandbox2", Policy: "web", State: models.StateRunning, Address: netip.MustParsePrefix("10.87.0.2/16")},
	}

	chains, err := New(s, records, gateway, nameservers, fakeResolver{}).Chains(t.Context())
	if err != nil {
		t.Fatalf("Chains: %v", err)
	}
	if len(chains) != 1 || len(chains[0].Rules) != 1 || chains[0].Rules[0].Action != models.ActionDeny {
		t.Fatalf("Chains = %+v, want only sandbox2's chain on the address the failed sandbox gave back", chains)
	}
}

func TestCompilesRefusesANameThatDoesNotResolve(t *testing.T) {
	svc := New(newStore(t), fakeRecords{}, gateway, nameservers, fakeResolver{"api.example.com": {netip.MustParseAddr("93.184.216.34")}})

	good := models.Policy{Name: "web", Rules: []models.Rule{
		mustRule(t, models.ActionAllow, "api.example.com"),
		mustRule(t, models.ActionAllow, "*.example.com"),
		mustRule(t, models.ActionAllow, "suffix:example.org"),
		mustRule(t, models.ActionAllow, "203.0.113.0/24"),
		mustRule(t, models.ActionDeny, "any"),
	}}
	if err := svc.Compiles(t.Context(), good); err != nil {
		t.Errorf("Compiles refused a policy whose one name resolves: %v", err)
	}

	bad := models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionDeny, "gone.example.com")}}
	err := svc.Compiles(t.Context(), bad)
	if err == nil || !strings.Contains(err.Error(), "deny gone.example.com") {
		t.Errorf("Compiles = %v, want the rule that names the host", err)
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

	got, err := New(s, nil, gateway, nameservers, fakeResolver{}).Effective(models.Sandbox{ID: "sandbox1", Policy: "addr"})
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}

	var shape []string
	for _, rule := range got.Rules {
		shape = append(shape, string(rule.Action)+" "+string(rule.Destination.Kind)+":"+rule.Destination.Value+" "+rule.Protocol+" "+rule.Implied)
	}
	want := []string{
		"allow cidr:10.87.0.1 udp dns rule",
		"allow cidr:10.87.0.1 tcp dns rule",
		"allow cidr:203.0.113.7 tcp ",
		"allow group:dns  ",
		"deny group:any  ",
	}
	if !slices.Equal(shape, want) {
		t.Errorf("Effective = %v, want %v", shape, want)
	}
}

// Resolves is what the view says, so it must agree with DecideName, which the resolver asks per name.
func TestResolvesAgreesWithTheFirstMatchingRule(t *testing.T) {
	s := newStore(t)
	svc := New(s, nil, gateway, nameservers, fakeResolver{})
	names := []string{"api.example.com", "www.example.com", "example.com", "any.example.net"}

	for _, tc := range []struct {
		name  string
		rules []models.Rule
		want  bool
	}{
		{"wide", []models.Rule{mustRule(t, models.ActionAllow, "any")}, true},
		{"reversed", []models.Rule{mustRule(t, models.ActionDeny, "any"), mustRule(t, models.ActionAllow, "any")}, false},
		{"shadowed", []models.Rule{mustRule(t, models.ActionDeny, "any"), mustRule(t, models.ActionAllow, "api.example.com")}, false},
		{"named", []models.Rule{mustRule(t, models.ActionAllow, "api.example.com"), mustRule(t, models.ActionDeny, "any")}, true},
		{"asked", []models.Rule{mustRule(t, models.ActionDeny, "any tcp:22"), mustRule(t, models.ActionAllow, "dns"), mustRule(t, models.ActionDeny, "any")}, true},
		{"half", []models.Rule{mustRule(t, models.ActionDeny, "any tcp:443"), mustRule(t, models.ActionAllow, "any")}, true},
		{"one", []models.Rule{mustRule(t, models.ActionDeny, "api.example.com"), mustRule(t, models.ActionAllow, "any")}, true},
		{"web", []models.Rule{mustRule(t, models.ActionAllow, "any tcp:443")}, false},
		{"narrow", []models.Rule{mustRule(t, models.ActionDeny, "api.example.com"), mustRule(t, models.ActionAllow, "api.example.com")}, false},
		{"pattern", []models.Rule{mustRule(t, models.ActionDeny, "api.example.com"), mustRule(t, models.ActionAllow, "*.example.com")}, true},
		{"apex", []models.Rule{mustRule(t, models.ActionDeny, "*.example.com"), mustRule(t, models.ActionAllow, "suffix:example.com")}, true},
		{"spelled", []models.Rule{mustRule(t, models.ActionDeny, "x.example.com"), mustRule(t, models.ActionAllow, "*.example.com")}, true},
		{"closed", []models.Rule{mustRule(t, models.ActionDeny, "example.com"), mustRule(t, models.ActionDeny, "*.example.com"), mustRule(t, models.ActionAllow, "suffix:example.com")}, false},
	} {
		policy := models.Policy{Name: tc.name, Rules: tc.rules}
		if err := s.Set(policy); err != nil {
			t.Fatal(err)
		}

		if got := Resolves(policy); got != tc.want {
			t.Errorf("Resolves(%s) = %v, want %v", tc.name, got, tc.want)
		}

		allowed := false
		for _, name := range names {
			got, err := svc.DecideName(models.Sandbox{ID: "sandbox1", Policy: tc.name}, name)
			if err != nil {
				t.Fatalf("DecideName(%s under %s): %v", name, tc.name, err)
			}
			allowed = allowed || got.Action == models.ActionAllow
		}
		if allowed != tc.want {
			t.Errorf("DecideName under %s allows a name: %v, want %v", tc.name, allowed, tc.want)
		}
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
