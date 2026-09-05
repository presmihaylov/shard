package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/presmihaylov/shard/models"
)

// LostReason is what a record says once the daemon found no process and no snapshot behind it.
const LostReason = "daemon restarted and found no process"

// ReconcileAll makes the records agree with the substrate, before the daemon serves its first verb.
// It corrects a record and never deletes one, and it reports one line per record it corrected.
func (s *Service) ReconcileAll(ctx context.Context, sandboxes []models.Sandbox, report func(string)) error {
	var errs []error
	running := 0

	for _, sb := range sandboxes {
		state, err := s.reconcileOne(ctx, sb, report)
		if err != nil {
			errs = append(errs, err)

			continue
		}
		if state == models.StateRunning {
			running++
		}
	}

	// Host netfilter is the policy of record, and nothing re-applied it while the last daemon was down.
	if running > 0 {
		if err := s.cfg.Network.ReapplyAll(ctx); err != nil {
			errs = append(errs, fmt.Errorf("re-apply the host rules for %d running sandboxes: %w", running, err))
		}
	}

	return errors.Join(errs...)
}

// reconcileOne corrects one record against the substrate and answers the state it left it in.
func (s *Service) reconcileOne(ctx context.Context, sb models.Sandbox, report func(string)) (models.State, error) {
	status, err := s.cfg.Provider.Status(ctx, sb.ID)
	if err != nil {
		return "", fmt.Errorf("ask %s about sandbox %s: %w", s.cfg.Provider.Name(), sb.ID, err)
	}

	state := reconciled(sb, status)
	if state == sb.State {
		return state, nil
	}

	if state == models.StateRunning {
		if err := RecordRunning(ctx, s.cfg.Repo, s.cfg.Provider, sb.ID, false); err != nil {
			return "", err
		}
		report(fmt.Sprintf("sandbox %s said %s and the substrate holds its process %d: the record now says running", sb.ID, sb.State, status.PID))

		return state, nil
	}

	err = s.cfg.Repo.Update(sb.ID, func(rec *models.Sandbox) error {
		rec.State = models.StateStopped
		rec.PID = 0
		rec.StoppedReason = LostReason

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("sandbox %s is gone but its record was not updated: %w", sb.ID, err)
	}
	report(fmt.Sprintf("sandbox %s said %s and nothing runs behind it: the record now says stopped, %s", sb.ID, sb.State, LostReason))

	return state, nil
}

// reconciled is the state the record should hold: what the substrate says, and for a paused one what
// the snapshot on disk says, because a checkpoint holds no process and resume still brings it back.
func reconciled(sb models.Sandbox, status models.Status) models.State {
	if status.Alive() {
		return models.StateRunning
	}

	if sb.State == models.StatePaused && hasCheckpoint(sb.Snapshot) {
		return models.StatePaused
	}

	if sb.State == models.StateRunning || sb.State == models.StatePaused {
		return models.StateStopped
	}

	// A created record never ran, and a stopped one is already right.
	return sb.State
}

func hasCheckpoint(dir string) bool {
	if dir == "" {
		return false
	}

	_, err := os.Stat(filepath.Join(dir, checkpointFile))

	return err == nil
}
