package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/secret"
)

// SecretHolders names the sandboxes whose record grants the secret. Every guard that asks who still
// sees a placeholder asks this one, so a rotation, an rm and an ungrant can never disagree.
func SecretHolders(repo Reader, name string) ([]string, error) {
	sandboxes, unreadable := repo.List()
	// A record that does not read back may name the secret, so nothing can say it is free.
	if unreadable != nil {
		return nil, fmt.Errorf("cannot tell which sandboxes hold the secret: %w", unreadable)
	}

	var holders []string
	for _, sb := range sandboxes {
		if slices.Contains(sb.Secrets, name) {
			holders = append(holders, sb.ID)
		}
	}

	return holders, nil
}

// GrantSecret hands a sandbox the placeholder of a stored secret, the way a create with --secret does.
// The value stays on the host: the proxy substitutes it on a request bound for a granted host.
func (s *Service) GrantSecret(ctx context.Context, ref, name string) (models.Sandbox, error) {
	id, sb, unlock, err := s.held(ref, name, "grant")
	if err != nil {
		return models.Sandbox{}, err
	}
	defer unlock()

	// The record is the source of truth, so a grant it already names is the outcome asked for.
	if slices.Contains(sb.Secrets, name) {
		return sb, nil
	}

	sec, err := s.cfg.Secrets.Get(name)
	if errors.Is(err, secret.ErrNotFound) {
		return models.Sandbox{}, &RequestError{Err: fmt.Errorf("secret %s does not exist: run shard secret set --to <host> %s first", name, name)}
	}
	if err != nil {
		return models.Sandbox{}, err
	}

	proxyCA, err := s.proxyCA()
	if err != nil {
		return models.Sandbox{}, err
	}

	b, err := s.bundle(id)
	if err != nil {
		return models.Sandbox{}, err
	}

	// Before any write: a refused grant must leave the bundle byte for byte as it found it.
	if err := b.CanSetEnv(name); err != nil {
		return models.Sandbox{}, &RequestError{Err: fmt.Errorf("sandbox %s cannot be granted secret %s: %w", id, name, err)}
	}

	if err := b.TrustProxy(proxyCA); err != nil {
		return models.Sandbox{}, err
	}

	if err := b.SetEnv(name, sec.Placeholder); err != nil {
		return models.Sandbox{}, err
	}

	err = s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		if !slices.Contains(sb.Secrets, name) {
			sb.Secrets = append(sb.Secrets, name)
		}

		return nil
	})
	if err != nil {
		return models.Sandbox{}, err
	}

	// The grant fronts the sandbox, so its web ports must reach the proxy before the guest runs again.
	if err := s.cfg.Network.Reapply(ctx, id); err != nil {
		return models.Sandbox{}, err
	}

	return s.record(id)
}

// UngrantSecret takes the placeholder back. The proxy CA stays: it is the image roots plus one
// certificate, and a sandbox that trusts it reaches no host the policy does not allow.
func (s *Service) UngrantSecret(ctx context.Context, ref, name string) (models.Sandbox, error) {
	id, _, unlock, err := s.held(ref, name, "ungrant")
	if err != nil {
		return models.Sandbox{}, err
	}
	defer unlock()

	b, err := s.bundle(id)
	if err != nil {
		return models.Sandbox{}, err
	}

	// The bundle goes first and the record second, so a run that stops between the two is finished by the next.
	if err := b.RemoveEnv(name); err != nil {
		return models.Sandbox{}, err
	}

	err = s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.Secrets = slices.DeleteFunc(sb.Secrets, func(held string) bool { return held == name })

		return nil
	})
	if err != nil {
		return models.Sandbox{}, err
	}

	// The sandbox may hold nothing now, and then the host has a DNAT rule to take off it.
	if err := s.cfg.Network.Reapply(ctx, id); err != nil {
		return models.Sandbox{}, err
	}

	return s.record(id)
}

// held takes the sandbox lock and refuses the states whose environment is already live: a running guest
// holds it in its processes, and a paused one holds it in the snapshot.
func (s *Service) held(ref, name, verb string) (string, models.Sandbox, func(), error) {
	if err := secret.ValidName(name); err != nil {
		return "", models.Sandbox{}, nil, &RequestError{Err: err}
	}

	return s.holdCreatedOrStopped(ref, "secret "+verb+" takes a created or stopped sandbox: stop it first")
}

// holdCreatedOrStopped locks the sandbox and refuses every state a verb that rewrites the bundle cannot take.
func (s *Service) holdCreatedOrStopped(ref, fix string) (string, models.Sandbox, func(), error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return "", models.Sandbox{}, nil, err
	}

	unlock := s.lock(id)

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		unlock()

		return "", models.Sandbox{}, nil, err
	}

	if sb.State != models.StateCreated && sb.State != models.StateStopped {
		unlock()

		return "", models.Sandbox{}, nil, &StateError{ID: id, State: sb.State, Fix: fix}
	}

	return id, sb, unlock, nil
}

// bundle opens the bundle of a sandbox that is already built, which is where the guest environment lives.
func (s *Service) bundle(id string) (bundle.Bundle, error) {
	dir, err := s.cfg.Repo.Dir(id)
	if err != nil {
		return bundle.Bundle{}, err
	}

	return bundle.Open(dir)
}
