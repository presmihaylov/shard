// Package broker decides every request the proxy carries: which sandbox sent it, whether its policy
// lets it go, where it goes, and which secret value replaces which placeholder on the way out.
package broker

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/secret"
)

// Records is the part of the sandbox repository the broker reads.
type Records interface {
	List() ([]models.Sandbox, error)
}

// Policies is the part of the egress service the broker reads: what a sandbox may reach, and what a name is.
type Policies interface {
	Effective(sb models.Sandbox) (egress.Effective, error)
	Resolve(ctx context.Context, host string) ([]netip.Addr, error)
}

// Secrets is the part of the secret store the broker reads. Value is read per request and never kept.
type Secrets interface {
	Get(name string) (secret.Secret, error)
	Value(name string) (string, error)
}

// Service is the proxy's Director.
type Service struct {
	records  Records
	policies Policies
	secrets  Secrets
}

// New wires a broker over the stores.
func New(records Records, policies Policies, secrets Secrets) *Service {
	return &Service{records: records, policies: policies, secrets: secrets}
}

// Route answers the proxy. The policy is read on every request, so a change lands at once, and the
// host is resolved here, so the address the policy checked is the one the proxy dials.
func (s *Service) Route(ctx context.Context, source netip.Addr, host string, port int) (proxy.Route, error) {
	sb, err := s.sandboxAt(source)
	if err != nil {
		return proxy.Route{}, err
	}

	effective, err := s.policies.Effective(sb)
	if err != nil {
		return proxy.Route{}, fmt.Errorf("sandbox %s: %w", sb.ID, err)
	}

	addrs, err := s.policies.Resolve(ctx, host)
	if err != nil {
		return proxy.Route{}, err
	}

	decision := egress.Decide(effective, host, port, addrs[0])
	if !decision.Allow {
		return proxy.Route{}, &proxy.Denied{Reason: decision.Reason}
	}

	subs, err := s.substitutions(sb, host)
	if err != nil {
		return proxy.Route{}, err
	}

	return proxy.Route{Target: netip.AddrPortFrom(addrs[0], uint16(port)), Rewrite: rewrite(subs)}, nil //nolint:gosec
}

// sandboxAt is the sandbox the lease gave the address to. The bridge table pins a port to its
// address, so the source is the sandbox and nothing else.
func (s *Service) sandboxAt(source netip.Addr) (models.Sandbox, error) {
	sandboxes, err := s.records.List()
	if err != nil {
		return models.Sandbox{}, err
	}

	for _, sb := range sandboxes {
		if sb.Address.IsValid() && sb.Address.Addr() == source {
			return sb, nil
		}
	}

	return models.Sandbox{}, &proxy.Denied{Reason: fmt.Sprintf("no sandbox holds the address %s", source)}
}

// substitutions is every grant the sandbox holds for host: the placeholder the guest sent, and the
// value that goes in its place. A grant for another host puts nothing in.
func (s *Service) substitutions(sb models.Sandbox, host string) ([]substitution, error) {
	var subs []substitution
	for _, name := range sb.Secrets {
		sec, err := s.secrets.Get(name)
		if errors.Is(err, secret.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}

		if !granted(sec, host) {
			continue
		}

		m, err := newMatcher(sec.Match)
		if err != nil {
			return nil, fmt.Errorf("secret %s match: %w", name, err)
		}

		value, err := s.secrets.Value(name)
		if err != nil {
			return nil, err
		}
		subs = append(subs, substitution{mock: sec.MockValue, value: value, headers: sec.Headers, match: m})
	}

	return subs, nil
}

func granted(sec secret.Secret, host string) bool {
	return slices.ContainsFunc(sec.Destinations, func(dest string) bool { return egress.MatchHost(dest, host) })
}
