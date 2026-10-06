package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// Records is the part of the sandbox repository the compiler reads.
type Records interface {
	List() ([]models.Sandbox, error)
}

// Resolver turns a name into addresses; the default asks the sandbox nameservers, so host and guest agree.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Service compiles every sandbox's policy into the chains the network service renders.
type Service struct {
	policies *Store
	records  Records
	resolver Resolver
	// gateway is where shard's resolver listens: the one DNS destination a policy sandbox may reach.
	gateway netip.Addr
	local   network.Local

	// mu guards good, the last chain each policy sandbox compiled whole, by id; it lives in memory, so a restart forgets it.
	mu   sync.Mutex
	good map[string]goodChain
}

// goodChain is a policy sandbox's last whole chain and the rules it compiled from.
type goodChain struct {
	rules []EffectiveRule
	chain network.Chain
}

// New wires a compiler over the stores. A nil resolver resolves through the nameservers.
func New(policies *Store, records Records, gateway netip.Addr, nameservers []netip.Addr, resolver Resolver) *Service {
	if resolver == nil {
		resolver = &net.Resolver{PreferGo: true, Dial: dialNameservers(nameservers)}
	}

	return &Service{policies: policies, records: records, resolver: resolver, gateway: gateway, good: map[string]goodChain{}}
}

// dialNameservers sends every lookup to the sandbox nameservers, and not to whatever the host resolves through.
func dialNameservers(nameservers []netip.Addr) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var errs []error
		for _, ns := range nameservers {
			var dialer net.Dialer

			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ns.String(), "53"))
			if err == nil {
				return conn, nil
			}
			errs = append(errs, err)
		}

		return nil, fmt.Errorf("no nameserver answered: %w", errors.Join(errs...))
	}
}

// Effective is what the host enforces for one sandbox: the policy's own rules, and the DNS they need. The record names the policy.
type Effective struct {
	// Missing is set when the record names a policy the store no longer holds: then everything is dropped.
	Missing bool            `json:"missing,omitempty"`
	Rules   []EffectiveRule `json:"rules"`
}

// EffectiveRule is one rule and, when the policy did not write it, what did.
type EffectiveRule struct {
	models.Rule
	// ID is the rule's place in the effective order, so the proxy and the host name the same rule in a log.
	ID      string `json:"id"`
	Implied string `json:"implied,omitempty" enum:"dns,dns-rule" doc:"Set on a rule the policy did not write: dns when a name rule opened DNS, dns-rule when a dns rule did."`
}

// Effective reads what the host enforces for the sandbox. A sandbox with no policy has no rules and reaches
// the internet and nothing private.
func (s *Service) Effective(sb models.Sandbox) (Effective, error) {
	if sb.Policy == "" {
		return Effective{}, nil
	}

	policy, err := s.policies.Get(sb.Policy)
	if errors.Is(err, ErrNotFound) {
		return Effective{Missing: true}, nil
	}
	if err != nil {
		return Effective{}, err
	}

	var rules []EffectiveRule
	// A dns rule and a name rule open the same door, and the operator should read which one did it.
	implied := "dns"
	for _, rule := range policy.Rules {
		if rule.Destination.Kind == models.DestinationGroup && rule.Destination.Value == GroupDNS {
			implied = "dns-rule"
		}
		rules = append(rules, EffectiveRule{Rule: rule})
	}

	// A name is no use to a guest that cannot resolve it, so a policy that names one opens DNS to shard's resolver.
	if OpensDNS(policy) {
		var dns []EffectiveRule
		for _, proto := range []string{"udp", "tcp"} {
			dns = append(dns, EffectiveRule{
				Rule: models.Rule{
					Action:      models.ActionAllow,
					Destination: models.Destination{Kind: models.DestinationCIDR, Value: s.gateway.String()},
					Protocol:    proto,
					Ports:       []int{53},
				},
				Implied: implied,
			})
		}
		rules = append(dns, rules...)
	}

	for i := range rules {
		rules[i].ID = strconv.Itoa(i + 1)
	}

	return Effective{Rules: rules}, nil
}

// Fronted says the sandbox's web traffic goes through the proxy, which a policy or a secret asks for.
func Fronted(sb models.Sandbox) bool {
	return sb.Policy != "" || len(sb.Secrets) != 0
}

// Chains compiles one chain per fronted sandbox with an address, since a lease outlives a stop; a held sandbox's *network.HeldChains comes with every chain.
func (s *Service) Chains(ctx context.Context) ([]network.Chain, error) {
	// nil log: the daemon tasks already name a bad record, so the compiler just skips it and goes on (SHARD-343).
	sandboxes, err := sandboxstate.ListReadable(s.records, nil)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var chains []network.Chain
	held := map[string]error{}
	compiled := map[string]bool{}
	for _, sb := range sandboxes {
		// A skip would leave a live sandbox no rule can match open, so the compile refuses it (SHARD-565).
		if Fronted(sb) && sb.State.Live() && !sb.Address.IsValid() {
			return nil, fmt.Errorf("sandbox %s is %s with no address on record, so no egress rule can guard it", sb.ID, sb.State)
		}
		// A failed create is terminal and never runs, and its teardown may have given its address to another sandbox.
		if !Fronted(sb) || !sb.Address.IsValid() || sb.State == models.StateFailed {
			continue
		}

		chain := network.Chain{Address: sb.Address.Addr(), Policy: sb.Policy != ""}
		if !chain.Policy {
			chains = append(chains, chain)

			continue
		}

		effective, err := s.Effective(sb)
		if err != nil {
			return nil, fmt.Errorf("sandbox %s: %w", sb.ID, err)
		}

		compiled[sb.ID] = true
		whole, err := s.compileChain(ctx, chain, effective.Rules)
		if err == nil {
			s.good[sb.ID] = goodChain{rules: effective.Rules, chain: whole}
			chains = append(chains, whole)

			continue
		}
		// A shutdown fails every lookup at once, and that is no reason to hold anyone.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("sandbox %s policy %s: %w", sb.ID, sb.Policy, err)
		}

		// Old rules with old addresses could open what the policy now closes, so only an unchanged policy keeps its chain.
		last, found := s.good[sb.ID]
		if found && last.chain.Address == chain.Address && reflect.DeepEqual(last.rules, effective.Rules) {
			chains = append(chains, last.chain)
			held[sb.ID] = fmt.Errorf("policy %s, on its last good chain: %w", sb.Policy, err)

			continue
		}
		// A chain with no rules is closed: web goes to the proxy, DNS to the resolver, and the rest is dropped.
		chains = append(chains, chain)
		held[sb.ID] = fmt.Errorf("policy %s, on a closed chain: %w", sb.Policy, err)
	}

	for id := range s.good {
		if !compiled[id] {
			delete(s.good, id)
		}
	}

	if len(held) != 0 {
		return chains, &network.HeldChains{Errs: held}
	}

	return chains, nil
}

// compileChain resolves every rule into the chain, or none: a chain missing one rule may have lost a deny.
func (s *Service) compileChain(ctx context.Context, chain network.Chain, rules []EffectiveRule) (network.Chain, error) {
	for _, rule := range rules {
		compiled, err := s.compile(ctx, rule)
		if err != nil {
			return network.Chain{}, err
		}
		chain.Rules = append(chain.Rules, compiled)
	}

	return chain, nil
}

// Compiles resolves every name of the policy the way an apply does, so a policy no apply could take is never stored.
func (s *Service) Compiles(ctx context.Context, policy models.Policy) error {
	for _, rule := range policy.Rules {
		if _, err := s.compile(ctx, EffectiveRule{Rule: rule}); err != nil {
			return fmt.Errorf("rule %q: %w", FormatRule(rule), err)
		}
	}

	return nil
}

func (s *Service) compile(ctx context.Context, rule EffectiveRule) (network.Compiled, error) {
	compiled := network.Compiled{ID: rule.ID, Sum: RuleSum(rule.Rule), Action: rule.Action, Protocol: rule.Protocol, Ports: slices.Clone(rule.Ports)}

	switch rule.Destination.Kind {
	case models.DestinationCIDR:
		prefix, err := parseCIDR(rule.Destination.Value)
		if err != nil {
			return network.Compiled{}, err
		}
		compiled.Prefixes = []netip.Prefix{prefix}
	case models.DestinationGroup:
		compiled.Prefixes = slices.Clone(network.Groups[rule.Destination.Value])
	case models.DestinationDomain:
		// A wildcard is a shape, not a name: only the proxy can match it, like a suffix.
		if strings.Contains(rule.Destination.Value, "*") {
			break
		}
		prefixes, err := s.resolve(ctx, rule.Destination.Value)
		if err != nil {
			return network.Compiled{}, err
		}
		compiled.Prefixes = prefixes
	case models.DestinationDomainSuffix:
		// The proxy matches a suffix by name; the chain sees addresses, so the web ports never reach it.
	default:
		return network.Compiled{}, fmt.Errorf("the host cannot enforce a %s rule", rule.Destination.Kind)
	}

	return compiled, nil
}

// resolve is done on the host, at apply time, so a guest that answers its own lookups changes nothing.
func (s *Service) resolve(ctx context.Context, host string) ([]netip.Prefix, error) {
	addrs, err := s.resolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s through the sandbox nameservers: %w", host, err)
	}

	prefixes := make([]netip.Prefix, 0, len(addrs))
	for _, addr := range addrs {
		addr = addr.Unmap()
		if !addr.Is4() {
			continue
		}
		prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	if len(prefixes) == 0 {
		return nil, fmt.Errorf("resolve %s: no IPv4 address", host)
	}

	slices.SortFunc(prefixes, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })

	return slices.Compact(prefixes), nil
}
