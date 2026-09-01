// Package broker decides every request the proxy carries: which sandbox sent it, whether its policy
// lets it go, where it goes, and which secret value replaces which placeholder on the way out.
package broker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"time"

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

// Events is where every decision is written, so an operator can see what was refused and why.
type Events interface {
	Record(ev models.EgressEvent) error
}

// maxHost is the longest name DNS allows.
const maxHost = 253

// Service is the proxy's Director.
type Service struct {
	records  Records
	policies Policies
	secrets  Secrets
	events   Events
}

// New wires a broker over the stores.
func New(records Records, policies Policies, secrets Secrets, events Events) *Service {
	return &Service{records: records, policies: policies, secrets: secrets, events: events}
}

// Route answers the proxy. The policy is read on every request, so a change lands at once, and the
// host is resolved here, so the address the policy checked is the one the proxy dials.
func (s *Service) Route(ctx context.Context, source netip.Addr, host string, port int) (proxy.Route, error) {
	sb, err := s.sandboxAt(source)
	if err != nil {
		return proxy.Route{}, err
	}
	// Nothing longer is a name, and the log records the host, so the bound keeps a guest from filling the root.
	if len(host) > maxHost {
		return proxy.Route{}, &proxy.Denied{Reason: fmt.Sprintf("the host is longer than %d characters", maxHost)}
	}

	effective, err := s.policies.Effective(sb)
	if err != nil {
		return proxy.Route{}, fmt.Errorf("sandbox %s: %w", sb.ID, err)
	}

	addrs, err := s.policies.Resolve(ctx, host)
	if err != nil {
		// A name that does not resolve is a closed door, and the log says which one.
		return proxy.Route{}, errors.Join(err, s.record(sb, host, port, egress.Decision{ID: egress.RuleResolve, Reason: err.Error()}, netip.Addr{}))
	}

	decision := egress.Decide(effective, host, port, addrs[0])
	if err := s.record(sb, host, port, decision, addrs[0]); err != nil {
		return proxy.Route{}, err
	}
	if !decision.Allow {
		return proxy.Route{}, &proxy.Denied{Reason: decision.Reason}
	}

	subs, err := s.substitutions(sb, host)
	if err != nil {
		return proxy.Route{}, err
	}

	return proxy.Route{Target: netip.AddrPortFrom(addrs[0], uint16(port)), Rewrite: rewrite(subs)}, nil //nolint:gosec
}

func (s *Service) record(sb models.Sandbox, host string, port int, decision egress.Decision, addr netip.Addr) error {
	ev := models.EgressEvent{
		Time:        time.Now(),
		Sandbox:     sb.ID,
		Source:      models.EgressSourceProxy,
		Verdict:     models.ActionDeny,
		Protocol:    "tcp",
		Destination: net.JoinHostPort(host, strconv.Itoa(port)),
		Rule:        decision.ID,
		Reason:      decision.Reason,
	}
	if decision.Allow {
		ev.Verdict = models.ActionAllow
	}
	if addr.IsValid() {
		ev.Address = addr.String()
	}
	if decision.Rule != nil {
		ev.RuleText = decision.Rule.Text()
	}

	if err := s.events.Record(ev); err != nil {
		return fmt.Errorf("sandbox %s: %w", sb.ID, err)
	}

	return nil
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
