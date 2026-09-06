package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/secret"
)

// Records is the part of the sandbox repository the compiler reads.
type Records interface {
	List() ([]models.Sandbox, error)
}

// Grants is the part of the secret store the compiler reads: never a value, only where one may go.
type Grants interface {
	Get(name string) (secret.Secret, error)
}

// Resolver turns a name into addresses; the default asks the sandbox nameservers, so host and guest agree.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Service compiles every sandbox's policy into the chains the network service renders.
type Service struct {
	policies    *Store
	records     Records
	grants      Grants
	resolver    Resolver
	nameservers []netip.Addr
}

// New wires a compiler over the stores. A nil resolver resolves through the nameservers.
func New(policies *Store, records Records, grants Grants, nameservers []netip.Addr, resolver Resolver) *Service {
	if resolver == nil {
		resolver = &net.Resolver{PreferGo: true, Dial: dialNameservers(nameservers)}
	}

	return &Service{policies: policies, records: records, grants: grants, resolver: resolver, nameservers: nameservers}
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

// Effective is what the host enforces for one sandbox: the policy's rules behind what its grants imply.
type Effective struct {
	Policy string `json:"policy"`
	// Missing is set when the record names a policy the store no longer holds: then everything is dropped.
	Missing bool            `json:"missing,omitempty"`
	Rules   []EffectiveRule `json:"rules"`
}

// EffectiveRule is one rule and, when the policy did not write it, what did.
type EffectiveRule struct {
	models.Rule
	Implied string `json:"implied,omitempty"`
}

// Effective reads what the host enforces for the sandbox. A sandbox with no policy has no rules and reaches
// the internet and nothing private.
func (s *Service) Effective(sb models.Sandbox) (Effective, error) {
	if sb.Policy == "" {
		return Effective{}, nil
	}

	policy, err := s.policies.Get(sb.Policy)
	if errors.Is(err, ErrNotFound) {
		return Effective{Policy: sb.Policy, Missing: true}, nil
	}
	if err != nil {
		return Effective{}, err
	}

	var rules []EffectiveRule

	needsDNS := false
	for _, name := range sb.Secrets {
		grant, err := s.grants.Get(name)
		if errors.Is(err, secret.ErrNotFound) {
			// A secret removed with --force leaves the grant behind, and a grant of nothing implies nothing.
			continue
		}
		if err != nil {
			return Effective{}, err
		}

		needsDNS = true
		for _, dest := range grant.Destinations {
			rules = append(rules, EffectiveRule{
				Rule: models.Rule{
					Action:      models.ActionAllow,
					Destination: models.Destination{Kind: models.DestinationDomain, Value: dest},
					Protocol:    "tcp",
					Ports:       slices.Clone(webPorts),
				},
				Implied: "secret " + name,
			})
		}
	}

	for _, rule := range policy.Rules {
		if named(rule.Destination.Kind) {
			needsDNS = true
		}
		rules = append(rules, EffectiveRule{Rule: rule})
	}

	// A name is no use to a guest that cannot resolve it, so a policy that names one opens DNS to the nameservers.
	if needsDNS {
		var dns []EffectiveRule
		for _, ns := range s.nameservers {
			for _, proto := range []string{"udp", "tcp"} {
				dns = append(dns, EffectiveRule{
					Rule: models.Rule{
						Action:      models.ActionAllow,
						Destination: models.Destination{Kind: models.DestinationCIDR, Value: ns.String()},
						Protocol:    proto,
						Ports:       []int{53},
					},
					Implied: "dns",
				})
			}
		}
		rules = append(dns, rules...)
	}

	return Effective{Policy: sb.Policy, Rules: rules}, nil
}

// Fronted says the sandbox's web traffic goes through the proxy, which a policy or a secret asks for.
func Fronted(sb models.Sandbox) bool {
	return sb.Policy != "" || len(sb.Secrets) != 0
}

// Chains compiles one chain per fronted sandbox with an address; a lease outlives a stop, so the chain does too.
func (s *Service) Chains(ctx context.Context) ([]network.Chain, error) {
	sandboxes, err := s.records.List()
	if err != nil {
		return nil, err
	}

	var chains []network.Chain
	for _, sb := range sandboxes {
		if !Fronted(sb) || !sb.Address.IsValid() {
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

		for _, rule := range effective.Rules {
			compiled, err := s.compile(ctx, rule.Rule)
			// A granted host the operator did not write into the policy closes itself when it does not resolve.
			if err != nil && rule.Implied != "" {
				compiled = network.Compiled{Action: rule.Action, Protocol: rule.Protocol, Ports: slices.Clone(rule.Ports)}
				err = nil
			}
			if err != nil {
				return nil, fmt.Errorf("sandbox %s policy %s: %w", sb.ID, sb.Policy, err)
			}
			chain.Rules = append(chain.Rules, compiled)
		}
		chains = append(chains, chain)
	}

	return chains, nil
}

func (s *Service) compile(ctx context.Context, rule models.Rule) (network.Compiled, error) {
	compiled := network.Compiled{Action: rule.Action, Protocol: rule.Protocol, Ports: slices.Clone(rule.Ports)}

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
