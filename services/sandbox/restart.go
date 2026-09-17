package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/presmihaylov/shard/models"
)

// DefaultRestartBackoff is the first wait a policy that names none gets; retries default to unlimited.
const DefaultRestartBackoff = 1

// validRestart refuses a policy that is not one of the three, and settings for a policy that never starts again.
func validRestart(r models.RestartSpec) error {
	switch r.Policy {
	case models.RestartNo, models.RestartOnFailure, models.RestartAlways:
	default:
		return fmt.Errorf("restart.policy is no, on-failure or always, got %q", r.Policy)
	}
	if r.Retries < 0 || r.Backoff < 0 {
		return errors.New("restart.retries and backoff are counts and cannot be negative")
	}
	if r.Backoff > models.RestartBackoffCap {
		return fmt.Errorf("restart.backoff is in seconds and never grows past %d, got %d", models.RestartBackoffCap, r.Backoff)
	}
	if r.Policy == models.RestartAlways && r.Retries != 0 {
		return errors.New("restart.policy always never gives up, so it takes no retries")
	}
	if !r.Set() && (r.Retries != 0 || r.Backoff != 0) {
		return errors.New("restart.retries and backoff need a policy that starts again, and the request names none")
	}

	return nil
}

// withRestartDefaults fills what the request left at zero, and is nil for a policy that never starts again.
func withRestartDefaults(r *models.RestartSpec) *models.Restart {
	if r == nil || !r.Set() {
		return nil
	}

	filled := *r
	if filled.Backoff == 0 {
		filled.Backoff = DefaultRestartBackoff
	}

	return &models.Restart{RestartSpec: filled}
}

// restartSpecOf is what a create hands the supervisor, which is nothing for a record without a policy.
func restartSpecOf(r *models.Restart) models.RestartSpec {
	if r == nil {
		return models.RestartSpec{}
	}

	return r.RestartSpec
}

// freshRestart keeps the policy and drops the count, which is what a new run of the entrypoint starts from.
func freshRestart(r *models.Restart) *models.Restart {
	if r == nil {
		return nil
	}

	return &models.Restart{RestartSpec: r.RestartSpec}
}

// RecordRestarts copies what the supervisor counted onto every running record that has a policy, one report line per change.
func (s *Service) RecordRestarts(ctx context.Context, sandboxes []models.Sandbox, report func(string)) error {
	var errs []error
	for _, sb := range sandboxes {
		if !UnderRestartPolicy(sb) {
			continue
		}
		if err := s.recordRestarts(ctx, sb.ID, report); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// UnderRestartPolicy says the record is one the supervisor may be starting again right now.
func UnderRestartPolicy(sb models.Sandbox) bool {
	return sb.State == models.StateRunning && sb.Restart != nil
}

// recordRestarts reads the record again under the lock, because a stop may have landed since the list.
func (s *Service) recordRestarts(ctx context.Context, id string, report func(string)) error {
	unlock := s.lock(id)
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return err
	}
	if !UnderRestartPolicy(sb) {
		return nil
	}

	count, err := s.lastRestarts(ctx, sb)
	if err != nil {
		return err
	}
	// The record's pointer may be the one the update writes through, so what it held is copied first.
	before, retries := sb.Restart.RestartCount, sb.Restart.Retries
	if count.Count == before.Count && count.GaveUp == before.GaveUp {
		return nil
	}

	err = s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.Restart.RestartCount = count

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s was started again but its record was not updated: %w", id, err)
	}

	if count.Count != before.Count {
		report(entrypointRestartReport(id, count.Count, retries))
	}
	if count.GaveUp && !before.GaveUp {
		report(fmt.Sprintf("sandbox %s: the entrypoint exited again and the %d starts again the policy allows are spent", id, retries))
	}

	return nil
}

// entrypointRestartReport names the limit when the policy has one and leaves it off when the starts again are unlimited.
func entrypointRestartReport(id string, count, retries int) string {
	if retries > 0 {
		return fmt.Sprintf("sandbox %s: the entrypoint was started again, %d of %d", id, count, retries)
	}

	return fmt.Sprintf("sandbox %s: the entrypoint was started again, %d", id, count)
}

// lastRestarts asks the supervisor's count for a record that has a policy, and is zero for one that has none.
func (s *Service) lastRestarts(ctx context.Context, sb models.Sandbox) (models.RestartCount, error) {
	if sb.Restart == nil {
		return models.RestartCount{}, nil
	}

	count, err := s.cfg.Provider.Restarts(ctx, sb.ID)
	if err != nil {
		return models.RestartCount{}, fmt.Errorf("read the restart count of sandbox %s: %w", sb.ID, err)
	}

	return count, nil
}
