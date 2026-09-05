package egress

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/network"
)

// Decision is what the proxy does with one request, and the rule that said so.
type Decision struct {
	Action models.Action
	// Rule is empty when no rule was asked: the floor, a missing policy or no policy at all decided.
	Rule   EffectiveRule
	Reason string
}

// Decide judges one tcp connection from sb to host, which resolved to addr, in the order Effective lists
// the rules. The proxy asks it, so a name rule matches here by name where the host chains cannot.
func (s *Service) Decide(sb models.Sandbox, host string, port int, addr netip.Addr) (Decision, error) {
	// The floor comes before every policy, on the host and here, so no name opens what the host hides.
	if slices.ContainsFunc(network.Private, func(p netip.Prefix) bool { return p.Contains(addr) }) {
		return Decision{Action: models.ActionDeny, Reason: fmt.Sprintf("%s resolves to %s, which is private", host, addr)}, nil
	}

	if sb.Policy == "" {
		return Decision{Action: models.ActionAllow, Reason: "sandbox " + sb.ID + " has no policy"}, nil
	}

	effective, err := s.Effective(sb)
	if err != nil {
		return Decision{}, fmt.Errorf("sandbox %s: %w", sb.ID, err)
	}
	if effective.Missing {
		return Decision{Action: models.ActionDeny, Reason: "policy " + sb.Policy + " does not exist"}, nil
	}

	for _, rule := range effective.Rules {
		if matches(rule.Rule, host, port, addr) {
			return Decision{Action: rule.Action, Rule: rule, Reason: "the first matching rule of policy " + sb.Policy}, nil
		}
	}

	return Decision{Action: models.ActionDeny, Reason: "no rule of policy " + sb.Policy + " matches " + host}, nil
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
