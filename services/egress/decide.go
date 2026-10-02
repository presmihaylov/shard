package egress

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/dns"
	"github.com/presmihaylov/shard/services/network"
)

// Decision is what the proxy does with one request, and the rule that said so.
type Decision struct {
	Action models.Action
	// Rule is empty when no rule was asked: the floor, a missing policy or no policy at all decided.
	Rule EffectiveRule
	// ID names what decided, so a log line from the proxy and one from the host point at the same rule.
	ID     string
	Reason string
}

// Decide judges one tcp connection from sb to host, which resolved to addr, in the order Effective lists
// the rules. The proxy asks it, so a name rule matches here by name where the host chains cannot.
func (s *Service) Decide(sb models.Sandbox, host string, port int, addr netip.Addr) (Decision, error) {
	// The floor comes before every policy, on the host and here, so no name opens what the host hides.
	// It reads the resolved address on purpose: on the Host header a private-resolving name would pass.
	if slices.ContainsFunc(network.Private, func(p netip.Prefix) bool { return p.Contains(addr) }) {
		// The reason drops the resolved address: the 403 body reaches the guest, and the egress log keeps the address (SHARD-342).
		return Decision{Action: models.ActionDeny, ID: network.RulePrivate, Reason: host + " resolves to a private address"}, nil
	}
	// The proxy dials from the host, where no chain refuses the host's own addresses, so what the floor misses of them is refused here.
	if s.local.Contains(addr) {
		return Decision{Action: models.ActionDeny, ID: network.RuleLocal, Reason: host + " resolves to an address local to the host"}, nil
	}

	if sb.Policy == "" {
		return Decision{Action: models.ActionAllow, ID: network.RuleNone, Reason: "sandbox " + sb.ID + " has no policy"}, nil
	}

	effective, err := s.Effective(sb)
	if err != nil {
		return Decision{}, fmt.Errorf("sandbox %s: %w", sb.ID, err)
	}
	if effective.Missing {
		return Decision{Action: models.ActionDeny, ID: network.RuleMissing, Reason: "policy " + sb.Policy + " does not exist"}, nil
	}

	for _, rule := range effective.Rules {
		if matches(rule.Rule, host, port, addr) {
			return Decision{Action: rule.Action, Rule: rule, ID: rule.ID, Reason: "the first matching rule of policy " + sb.Policy}, nil
		}
	}

	return Decision{Action: models.ActionDeny, ID: network.RuleDefault, Reason: "no rule of policy " + sb.Policy + " matches " + host}, nil
}

// DecideName judges a question by its name alone, so a name the policy never allows is refused unresolved and carries nothing out or in.
func (s *Service) DecideName(sb models.Sandbox, name string) (Decision, error) {
	if sb.Policy == "" {
		return Decision{Action: models.ActionAllow, ID: network.RuleNone, Reason: "sandbox " + sb.ID + " has no policy"}, nil
	}

	effective, err := s.Effective(sb)
	if err != nil {
		return Decision{}, fmt.Errorf("sandbox %s: %w", sb.ID, err)
	}
	if effective.Missing {
		return Decision{Action: models.ActionDeny, ID: network.RuleMissing, Reason: "policy " + sb.Policy + " does not exist"}, nil
	}

	for _, rule := range effective.Rules {
		if matchesName(rule.Rule, name) {
			return Decision{Action: rule.Action, Rule: rule, ID: rule.ID, Reason: "the first matching rule of policy " + sb.Policy}, nil
		}
	}

	return Decision{Action: models.ActionDeny, ID: network.RuleDefault, Reason: "no rule of policy " + sb.Policy + " matches " + name}, nil
}

// Unresolved judges an http request by name, before any resolver is asked, so a host the policy never allows
// is refused without a lookup and its reason names only the host (SHARD-342). The bool is true when the deny
// is final; false means the verdict needs the resolved address, so the caller resolves and asks Decide.
func (s *Service) Unresolved(sb models.Sandbox, host string, port int) (Decision, bool, error) {
	if sb.Policy == "" {
		return Decision{}, false, nil
	}

	effective, err := s.Effective(sb)
	if err != nil {
		return Decision{}, false, fmt.Errorf("sandbox %s: %w", sb.ID, err)
	}
	if effective.Missing {
		return Decision{Action: models.ActionDeny, ID: network.RuleMissing, Reason: "policy " + sb.Policy + " does not exist"}, true, nil
	}

	for _, rule := range effective.Rules {
		if nameDecidesHTTP(rule.Rule, host, port) {
			// A name rule decides by name; an allow still faces the floor, so only its deny is final here.
			if rule.Action == models.ActionDeny {
				return Decision{Action: models.ActionDeny, Rule: rule, ID: rule.ID, Reason: "the first matching rule of policy " + sb.Policy}, true, nil
			}

			return Decision{}, false, nil
		}
		// An allow rule an address could match is the only way a lookup turns the default deny into an allow.
		if mightAllowByAddress(rule.Rule, port) {
			return Decision{}, false, nil
		}
	}

	return Decision{Action: models.ActionDeny, ID: network.RuleDefault, Reason: "no rule of policy " + sb.Policy + " matches " + host}, true, nil
}

// anyLeavesDNS says an any rule leaves port 53 open, which is what let a guest resolve before the resolver.
func anyLeavesDNS(rule models.Rule) bool {
	return rule.Destination.Kind == models.DestinationGroup && rule.Destination.Value == GroupAny && (len(rule.Ports) == 0 || slices.Contains(rule.Ports, dns.Port))
}

// resolves is DecideName's walk over the stored rules: the implied rules are addresses, which never match a name.
func resolves(rules []models.Rule, name string) bool {
	for _, rule := range rules {
		if matchesName(rule, name) {
			return rule.Action == models.ActionAllow
		}
	}

	return false
}

// samples are names a rule matches, built on a label no rule spells, so only a deny as wide as the rule shadows them all.
func samples(rule models.Rule, fresh string) []string {
	switch rule.Destination.Kind {
	case models.DestinationGroup:
		return []string{fresh}
	case models.DestinationDomain:
		return []string{strings.ReplaceAll(rule.Destination.Value, "*", fresh)}
	case models.DestinationDomainSuffix:
		return []string{rule.Destination.Value, fresh + "." + rule.Destination.Value}
	}

	return nil
}

func freshLabel(rules []models.Rule) string {
	label := "x"
	for slices.ContainsFunc(rules, func(rule models.Rule) bool { return slices.Contains(strings.Split(rule.Destination.Value, "."), label) }) {
		label += "x"
	}

	return label
}

// matchesName says whether a rule speaks for a name alone: an address rule cannot, and only a deny that closes both web ports refuses a lookup.
func matchesName(rule models.Rule, name string) bool {
	if rule.Action == models.ActionDeny && !closesName(rule) {
		return false
	}

	switch rule.Destination.Kind {
	case models.DestinationGroup:
		return rule.Destination.Value == GroupDNS || anyLeavesDNS(rule)
	case models.DestinationDomain:
		return MatchHost(rule.Destination.Value, name)
	case models.DestinationDomainSuffix:
		return name == rule.Destination.Value || strings.HasSuffix(name, "."+rule.Destination.Value)
	}

	return false
}

// closesName is a deny no name rule survives: a name rule is tcp on the web ports, so a deny that leaves one open leaves the name in use.
func closesName(rule models.Rule) bool {
	if rule.Protocol != "" && rule.Protocol != "tcp" {
		return false
	}
	if len(rule.Ports) == 0 {
		return true
	}

	for _, port := range webPorts {
		if !slices.Contains(rule.Ports, port) {
			return false
		}
	}

	return true
}

// nameDecidesHTTP settles an http request by name and port alone: a named host, or the any group; never the dns group or a bare address, which need resolving.
func nameDecidesHTTP(rule models.Rule, host string, port int) bool {
	if rule.Protocol != "" && rule.Protocol != "tcp" {
		return false
	}
	if len(rule.Ports) != 0 && !slices.Contains(rule.Ports, port) {
		return false
	}

	switch rule.Destination.Kind {
	case models.DestinationDomain:
		return MatchHost(rule.Destination.Value, host)
	case models.DestinationDomainSuffix:
		return host == rule.Destination.Value || strings.HasSuffix(host, "."+rule.Destination.Value)
	case models.DestinationGroup:
		return rule.Destination.Value == GroupAny
	}

	return false
}

// mightAllowByAddress is true when an allow rule could match some resolved address on this port. Only such a
// rule lets a lookup turn the default deny into an allow, so the http path resolves for these and refuses
// every other unmatched host unresolved (SHARD-342). A dns group is the resolver, never a web host.
func mightAllowByAddress(rule models.Rule, port int) bool {
	if rule.Action != models.ActionAllow {
		return false
	}
	if rule.Protocol != "" && rule.Protocol != "tcp" {
		return false
	}
	if len(rule.Ports) != 0 && !slices.Contains(rule.Ports, port) {
		return false
	}

	switch rule.Destination.Kind {
	case models.DestinationCIDR:
		return true
	case models.DestinationGroup:
		return rule.Destination.Value != GroupDNS
	}

	return false
}

func matches(rule models.Rule, host string, port int, addr netip.Addr) bool {
	if rule.Protocol != "" && rule.Protocol != "tcp" {
		return false
	}
	if len(rule.Ports) != 0 && !slices.Contains(rule.Ports, port) {
		return false
	}

	switch rule.Destination.Kind {
	case models.DestinationCIDR:
		prefix, err := parseCIDR(rule.Destination.Value)

		return err == nil && prefix.Contains(addr)
	case models.DestinationGroup:
		return slices.ContainsFunc(network.Groups[rule.Destination.Value], func(p netip.Prefix) bool { return p.Contains(addr) })
	case models.DestinationDomain:
		return MatchHost(rule.Destination.Value, host)
	case models.DestinationDomainSuffix:
		return host == rule.Destination.Value || strings.HasSuffix(host, "."+rule.Destination.Value)
	}

	return false
}

// MatchHost says whether host is what pattern names: a leading *. is any depth under the apex and never
// the apex, any other * is exactly one label, and * alone is every host.
func MatchHost(pattern, host string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == host
	}

	want := strings.Split(pattern, ".")
	got := strings.Split(host, ".")

	if want[0] == "*" {
		want = want[1:]
		if len(got) <= len(want) {
			return false
		}
		got = got[len(got)-len(want):]
	}

	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i] != "*" && want[i] != got[i] {
			return false
		}
	}

	return true
}

// Lookup resolves host the way the host chains do, so the proxy judges and dials the address the policy saw.
func (s *Service) Lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	prefixes, err := s.resolve(ctx, host)
	if err != nil {
		return nil, err
	}

	addrs := make([]netip.Addr, 0, len(prefixes))
	for _, prefix := range prefixes {
		addrs = append(addrs, prefix.Addr())
	}

	return addrs, nil
}

// FormatRule spells a rule the way policy create takes it, so an error can quote it back.
func FormatRule(rule models.Rule) string {
	dest := rule.Destination.Value
	if rule.Destination.Kind == models.DestinationDomainSuffix {
		dest = "suffix:" + dest
	}

	text := string(rule.Action) + " " + dest
	if rule.Protocol == "" {
		return text
	}

	text += " " + rule.Protocol
	if len(rule.Ports) == 0 {
		return text
	}

	ports := make([]string, 0, len(rule.Ports))
	for _, port := range rule.Ports {
		ports = append(ports, strconv.Itoa(port))
	}

	return text + ":" + strings.Join(ports, ",")
}
