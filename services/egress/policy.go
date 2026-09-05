// Package egress compiles a sandbox's policy into what the host enforces. Host netfilter is the
// policy of record: a name is resolved here, on the host, and the guest's own resolver decides nothing.
package egress

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/secret"
)

// ErrNotFound is what a read of a policy the store does not hold returns. Match it with errors.Is.
var ErrNotFound = errors.New("policy not found")

const (
	dirPerm  = 0o750
	filePerm = 0o640
	maxRules = 256
	// maxPorts bounds what a range expands to, so a rule cannot turn into a set nft chokes on.
	maxPorts = 1024
)

// webPorts is what a domain rule may name: the proxy speaks HTTP and TLS and nothing else.
var webPorts = []int{80, 443}

// Store keeps one JSON file per policy under <root>/policies.
type Store struct {
	dir string
}

// NewStore prepares the policy directory.
func NewStore(dir string) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("the policy store needs an absolute path, got %q", dir)
	}

	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}

	return &Store{dir: dir}, nil
}

// Set writes the policy, validated first, so the store never holds one the host could not enforce.
func (s *Store) Set(policy models.Policy) error {
	if err := Validate(policy); err != nil {
		return err
	}

	blob, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return fmt.Errorf("encode policy %s: %w", policy.Name, err)
	}

	if err := store.WriteFile(s.path(policy.Name), blob, filePerm); err != nil {
		return fmt.Errorf("write policy %s: %w", policy.Name, err)
	}

	return nil
}

// Get reads one policy.
func (s *Store) Get(name string) (models.Policy, error) {
	if err := ValidName(name); err != nil {
		return models.Policy{}, err
	}

	blob, err := os.ReadFile(s.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return models.Policy{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return models.Policy{}, fmt.Errorf("read policy %s: %w", name, err)
	}

	var policy models.Policy
	if err := json.Unmarshal(blob, &policy); err != nil {
		return models.Policy{}, fmt.Errorf("decode policy %s: %w", name, err)
	}

	// The file name is the identity, so a file edited by hand cannot answer for another name.
	policy.Name = name
	// A file edited by hand would otherwise render a rule nft refuses and break every Ensure.
	if err := Validate(policy); err != nil {
		return models.Policy{}, fmt.Errorf("policy %s: %w", name, err)
	}

	return policy, nil
}

// List reads every policy, sorted by name.
func (s *Store) List() ([]models.Policy, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.dir, err)
	}

	policies := make([]models.Policy, 0, len(entries))
	for _, entry := range entries {
		name, isPolicy := strings.CutSuffix(entry.Name(), ".json")
		if entry.IsDir() || !isPolicy || ValidName(name) != nil {
			continue
		}

		policy, err := s.Get(name)
		if err != nil {
			return nil, err
		}

		policies = append(policies, policy)
	}

	return policies, nil
}

// Remove deletes the policy. It is idempotent.
func (s *Store) Remove(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}

	if err := os.Remove(s.path(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove policy %s: %w", name, err)
	}

	return store.SyncDir(s.dir)
}

func (s *Store) path(name string) string { return filepath.Join(s.dir, name+".json") }

var nameShape = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidName refuses a policy name that is not one plain lowercase word.
func ValidName(name string) error {
	const maxChars = 64

	if name == "" {
		return errors.New("the policy name is empty")
	}
	if len(name) > maxChars {
		return fmt.Errorf("the policy name %q is longer than %d characters", name, maxChars)
	}
	if !nameShape.MatchString(name) {
		return fmt.Errorf("the policy name %q is not lowercase letters, digits and -", name)
	}

	return nil
}

// Validate refuses a policy the host could not enforce exactly as written.
func Validate(policy models.Policy) error {
	if err := ValidName(policy.Name); err != nil {
		return err
	}
	if len(policy.Rules) > maxRules {
		return fmt.Errorf("policy %s has %d rules, and %d is the most a policy may hold", policy.Name, len(policy.Rules), maxRules)
	}

	for i, rule := range policy.Rules {
		if err := validRule(rule); err != nil {
			return fmt.Errorf("policy %s rule %d: %w", policy.Name, i+1, err)
		}
	}

	return nil
}

func validRule(rule models.Rule) error {
	if rule.Action != models.ActionAllow && rule.Action != models.ActionDeny {
		return fmt.Errorf("the action %q is not allow or deny", rule.Action)
	}

	switch rule.Protocol {
	case "", "tcp", "udp":
	default:
		return fmt.Errorf("the protocol %q is not tcp or udp", rule.Protocol)
	}
	if rule.Protocol == "" && len(rule.Ports) != 0 {
		return errors.New("ports need a protocol: name tcp or udp")
	}
	for _, port := range rule.Ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("the port %d is not between 1 and 65535", port)
		}
	}
	if len(rule.Ports) > maxPorts {
		return fmt.Errorf("the rule names %d ports, and %d is the most one may", len(rule.Ports), maxPorts)
	}

	return validDestination(rule)
}

func validDestination(rule models.Rule) error {
	dest := rule.Destination

	switch dest.Kind {
	case models.DestinationCIDR:
		_, err := parseCIDR(dest.Value)

		return err
	case models.DestinationGroup:
		if _, known := network.Groups[dest.Value]; !known {
			return fmt.Errorf("the group %q is not any", dest.Value)
		}

		return nil
	case models.DestinationDomain, models.DestinationDomainSuffix:
		if err := validHostValue(dest); err != nil {
			return err
		}
		// A name is enforced through the proxy, which speaks HTTP and TLS: on any other port it would be an address guess.
		if rule.Protocol != "tcp" {
			return fmt.Errorf("a %s rule is tcp only, got %q", dest.Kind, rule.Protocol)
		}
		if len(rule.Ports) == 0 {
			return fmt.Errorf("a %s rule names ports 80 and 443 only, got none: without one it would open every tcp port", dest.Kind)
		}
		for _, port := range rule.Ports {
			if !slices.Contains(webPorts, port) {
				return fmt.Errorf("a %s rule may name ports 80 and 443 only, got %d: a raw port takes an address or a prefix", dest.Kind, port)
			}
		}

		return nil
	}

	return fmt.Errorf("the destination kind %q is not cidr, domain or group", dest.Kind)
}

// parseCIDR takes a prefix or a bare address, and refuses IPv6, which the sandbox network does not carry.
func parseCIDR(value string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		addr, addrErr := netip.ParseAddr(value)
		if addrErr != nil {
			return netip.Prefix{}, fmt.Errorf("%q is not an address or a prefix", value)
		}
		prefix = netip.PrefixFrom(addr, addr.BitLen())
	}
	if !prefix.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%q is not IPv4, which is all a sandbox carries", value)
	}

	return prefix.Masked(), nil
}

// ParseRule reads one rule as the CLI spells it: <destination> [tcp|udp[:<ports>]], with ports as
// a comma list of numbers and ranges, so "api.example.com", "10.0.0.0/8 tcp:22,8000-8100" or "any".
func ParseRule(action models.Action, text string) (models.Rule, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 || len(fields) > 2 {
		return models.Rule{}, fmt.Errorf("%q is not <destination> [tcp|udp[:<ports>]]", text)
	}

	dest, err := parseDestination(fields[0])
	if err != nil {
		return models.Rule{}, err
	}

	rule := models.Rule{Action: action, Destination: dest}

	if len(fields) == 2 {
		proto, ports, _ := strings.Cut(fields[1], ":")
		rule.Protocol = proto

		parsed, err := parsePorts(ports)
		if err != nil {
			return models.Rule{}, err
		}
		rule.Ports = parsed
	}

	// A name rule with no port is the web, which is all a name rule may name anyway.
	if named(rule.Destination.Kind) && rule.Protocol == "" {
		rule.Protocol = "tcp"
	}
	if named(rule.Destination.Kind) && rule.Protocol == "tcp" && len(rule.Ports) == 0 {
		rule.Ports = slices.Clone(webPorts)
	}

	if err := validRule(rule); err != nil {
		return models.Rule{}, err
	}

	return rule, nil
}

// named says the rule matches a host name, which only the proxy can see.
func named(kind models.DestinationKind) bool {
	return kind == models.DestinationDomain || kind == models.DestinationDomainSuffix
}

// parseDestination reads the kind from the shape: an address or a prefix is a cidr, any is the
// group, suffix: names a suffix, and everything else is a domain.
func parseDestination(text string) (models.Destination, error) {
	if text == "any" {
		return models.Destination{Kind: models.DestinationGroup, Value: "any"}, nil
	}
	if text == "private" || text == "group:private" {
		return models.Destination{}, errors.New("the private ranges are always blocked, and no rule changes that")
	}
	if _, err := netip.ParseAddr(text); err == nil {
		return models.Destination{Kind: models.DestinationCIDR, Value: text}, nil
	}
	if _, err := netip.ParsePrefix(text); err == nil {
		return models.Destination{Kind: models.DestinationCIDR, Value: text}, nil
	}
	if value, ok := strings.CutPrefix(text, "suffix:"); ok {
		return models.Destination{Kind: models.DestinationDomainSuffix, Value: value}, nil
	}
	if kind, _, found := strings.Cut(text, ":"); found && slices.Contains([]string{"cidr", "domain", "domain-suffix", "group"}, kind) {
		return models.Destination{}, fmt.Errorf("%q spells the old syntax: write the destination bare, as <host>, <cidr>, suffix:<name> or any", text)
	}

	return models.Destination{Kind: models.DestinationDomain, Value: text}, nil
}

func parsePorts(text string) ([]int, error) {
	if text == "" {
		return nil, nil
	}

	var ports []int
	for part := range strings.SplitSeq(text, ",") {
		first, last, isRange := strings.Cut(part, "-")

		from, err := strconv.Atoi(first)
		if err != nil {
			return nil, fmt.Errorf("the port %q is not a number", first)
		}
		to := from
		if isRange {
			if to, err = strconv.Atoi(last); err != nil {
				return nil, fmt.Errorf("the port %q is not a number", last)
			}
		}
		if to < from {
			return nil, fmt.Errorf("the port range %q runs backwards", part)
		}
		if to-from+1 > maxPorts {
			return nil, fmt.Errorf("the port range %q is wider than %d ports", part, maxPorts)
		}

		for port := from; port <= to; port++ {
			ports = append(ports, port)
		}
	}

	slices.Sort(ports)

	return slices.Compact(ports), nil
}

// validHostValue checks the name a rule carries; only a domain rule may carry a wildcard.
func validHostValue(dest models.Destination) error {
	if !strings.Contains(dest.Value, "*") {
		_, err := secret.ValidDestination(dest.Value)

		return err
	}
	if dest.Kind == models.DestinationDomainSuffix {
		return fmt.Errorf("a suffix rule takes no wildcard: %q already names every name under it", dest.Value)
	}
	if dest.Value == "*" {
		return nil
	}

	_, err := secret.ValidDestination(dest.Value)

	return err
}
