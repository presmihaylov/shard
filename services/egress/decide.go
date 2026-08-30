package egress

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/network"
)

// Decision is what the proxy does with one request; ID is the rule's index, or what stood in when none matched.
type Decision struct {
	Allow  bool
	Rule   *EffectiveRule
	ID     string
	Reason string
}

// What stands in for a rule index when no rule decided.
const (
	RuleNone    = "none"
	RulePrivate = "private"
	RuleMissing = "missing"
	RuleDefault = "default"
	RuleResolve = "resolve"
)

// Decide runs one TCP request through what the host enforces, by the name it carries and the address
// that name resolved to. It is the proxy's side of the policy, so it and the host's chain agree.
func Decide(eff Effective, host string, port int, addr netip.Addr) Decision {
	if eff.Policy == "" {
		if isPrivate(addr) {
			return Decision{ID: RulePrivate, Reason: fmt.Sprintf("%s resolves to %s, which is private", host, addr)}
		}

		return Decision{Allow: true, ID: RuleNone, Reason: "no policy"}
	}

	if eff.Missing {
		return Decision{ID: RuleMissing, Reason: fmt.Sprintf("policy %s no longer exists, so the sandbox reaches nothing", eff.Policy)}
	}

	for i := range eff.Rules {
		rule := &eff.Rules[i]
		if !matches(rule.Rule, host, port, addr) {
			continue
		}

		id := strconv.Itoa(i)
		if rule.Action != models.ActionAllow {
			return Decision{Rule: rule, ID: id, Reason: fmt.Sprintf("policy %s denies %s", eff.Policy, describe(rule))}
		}

		// A grant names a host the operator never wrote into the policy, so it is no leave to reach the host's own networks.
		if rule.Implied != "" && isPrivate(addr) {
			return Decision{Rule: rule, ID: RulePrivate, Reason: fmt.Sprintf("%s resolves to %s, which is private, and only the policy may open that", host, addr)}
		}

		return Decision{Allow: true, Rule: rule, ID: id, Reason: fmt.Sprintf("policy %s allows %s", eff.Policy, describe(rule))}
	}

	return Decision{ID: RuleDefault, Reason: fmt.Sprintf("policy %s has no rule for %s:%d", eff.Policy, host, port)}
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
		return within(network.Groups[rule.Destination.Value], addr)
	case models.DestinationDomain:
		return MatchHost(rule.Destination.Value, host)
	case models.DestinationDomainSuffix:
		return host == rule.Destination.Value || strings.HasSuffix(host, "."+rule.Destination.Value)
	}

	return false
}

func describe(rule *EffectiveRule) string {
	text := rule.Destination.Value
	if rule.Implied != "" {
		text += " (" + rule.Implied + ")"
	}

	return text
}

func isPrivate(addr netip.Addr) bool { return within(network.Groups["private"], addr) }

func within(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// MatchHost says whether host is what pattern names. A leading "*." matches any depth under the
// rest, any other "*" stands for exactly one label, and "*" alone is every host.
func MatchHost(pattern, host string) bool {
	if pattern == "*" {
		return true
	}
	if rest, ok := strings.CutPrefix(pattern, "*."); ok && !strings.Contains(rest, "*") {
		return len(host) > len(rest)+1 && strings.EqualFold(host[len(host)-len(rest)-1:], "."+rest)
	}

	patternLabels := strings.Split(pattern, ".")
	hostLabels := strings.Split(host, ".")
	if len(patternLabels) != len(hostLabels) {
		return false
	}
	for i, label := range patternLabels {
		if label != "*" && !strings.EqualFold(label, hostLabels[i]) {
			return false
		}
	}

	return true
}
