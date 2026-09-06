package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
)

// PolicyAttachRequest is the body of a policy PUT: the one policy the sandbox holds from its next start.
type PolicyAttachRequest struct {
	Policy string `json:"policy"`
}

// AttachPolicy gives a sandbox that already exists the policy a create with --policy would have given it.
// The host enforces it from the next start, and a sandbox holds one policy, so an attach replaces it.
func (s *Service) AttachPolicy(ctx context.Context, ref, name string) (models.Sandbox, error) {
	if name == "" {
		return models.Sandbox{}, &RequestError{Err: errors.New("policy attach takes a policy name")}
	}

	id, sb, unlock, err := s.holdForPolicy(ref, "attach")
	if err != nil {
		return models.Sandbox{}, err
	}
	defer unlock()

	// The record is the source of truth, so the policy it already names is the outcome asked for.
	if sb.Policy == name {
		return sb, nil
	}

	if _, err := s.cfg.Policies.Get(name); err != nil {
		return models.Sandbox{}, &RequestError{Err: err}
	}

	// An attach on a sandbox that held neither policy nor secret is what fronts it, so the CA goes first.
	if !egress.Fronted(sb) {
		if err := s.trustProxy(id); err != nil {
			return models.Sandbox{}, err
		}
	}

	return s.movePolicy(ctx, id, sb.Policy, name)
}

// DetachPolicy takes the policy back. The proxy CA stays, as it does after an ungrant, and the sandbox
// keeps its secrets and the fronting they ask for.
func (s *Service) DetachPolicy(ctx context.Context, ref string) (models.Sandbox, error) {
	id, sb, unlock, err := s.holdForPolicy(ref, "detach")
	if err != nil {
		return models.Sandbox{}, err
	}
	defer unlock()

	if sb.Policy == "" {
		return sb, nil
	}

	return s.movePolicy(ctx, id, sb.Policy, "")
}

// movePolicy writes the record and then puts the rules on the host, and puts the record back when the
// host refuses them: a record that names a policy the host does not enforce is the one outcome to avoid.
func (s *Service) movePolicy(ctx context.Context, id, was, want string) (models.Sandbox, error) {
	if err := s.writePolicy(id, want); err != nil {
		return models.Sandbox{}, err
	}

	// The revert is honest only because Reapply lands one ruleset through one nft -f: all of it or none.
	if err := s.cfg.Network.Reapply(ctx, id); err != nil {
		if revert := s.writePolicy(id, was); revert != nil {
			return models.Sandbox{}, fmt.Errorf("the host did not take the rules and the record did not go back: %w", errors.Join(err, revert))
		}

		return models.Sandbox{}, fmt.Errorf("the host did not take the rules, so the record is as it was: %w", err)
	}

	return s.record(id)
}

func (s *Service) writePolicy(id, name string) error {
	return s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.Policy = name

		return nil
	})
}

// trustProxy plants the proxy CA in the bundle, as a grant does. Running it again changes nothing.
func (s *Service) trustProxy(id string) error {
	proxyCA, err := s.proxyCA()
	if err != nil {
		return err
	}

	b, err := s.bundle(id)
	if err != nil {
		return err
	}

	return b.TrustProxy(proxyCA)
}

func (s *Service) holdForPolicy(ref, verb string) (string, models.Sandbox, func(), error) {
	return s.holdCreatedOrStopped(ref, "policy "+verb+" takes a created or stopped sandbox: stop it first")
}
