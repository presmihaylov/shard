package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/presmihaylov/shard/models"
)

// LostReason is what a record says once the daemon found no process and no snapshot behind it.
const LostReason = "daemon restarted and found no process"

// InterruptedReason is what a pending create's record says once the daemon restarted before it finished.
const InterruptedReason = "the daemon restarted before the create finished"

// ReconcileConcurrency bounds the startup probes in flight, so N frozen sandboxes cost about one budget, not N.
const ReconcileConcurrency = 16

// ReconcileAll makes the records agree with the substrate, before the daemon serves its first verb.
// It corrects a record and never deletes one, and it reports one line per record it corrected.
func (s *Service) ReconcileAll(ctx context.Context, sandboxes []models.Sandbox, report func(string)) error {
	// The probe is the slow part, so run every probe concurrently, then apply the corrections one at a time.
	probes := s.probeAll(ctx, sandboxes)

	var errs []error
	running := 0
	for i, sb := range sandboxes {
		state, err := s.applyReconcile(ctx, sb, probes[i].status, probes[i].err, report)
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

// probeResult is one sandbox's Status and the error its probe answered, held by the sandbox's index.
type probeResult struct {
	status models.Status
	err    error
}

// probeAll asks the substrate about every sandbox at once, up to ReconcileConcurrency, each under its own budget.
func (s *Service) probeAll(ctx context.Context, sandboxes []models.Sandbox) []probeResult {
	probes := make([]probeResult, len(sandboxes))
	sem := make(chan struct{}, ReconcileConcurrency)
	var wg sync.WaitGroup
	for i := range sandboxes {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			status, err := s.status(ctx, sandboxes[i].ID, "reconcile")
			probes[i] = probeResult{status: status, err: err}
		}(i)
	}
	wg.Wait()

	return probes
}

// applyReconcile corrects one record from its probe result and answers the state it left it in.
func (s *Service) applyReconcile(ctx context.Context, sb models.Sandbox, status models.Status, probeErr error, report func(string)) (models.State, error) {
	var timeout *SubstrateTimeoutError
	if errors.As(probeErr, &timeout) {
		// The probe budget bounds every Status, so a wedge stalls no boot; the liveness tick reconciles it later.
		report(fmt.Sprintf("sandbox %s: the substrate did not answer within %s, the record is left as it is and the liveness tick reconciles it", sb.ID, timeout.Budget))

		return sb.State, nil
	}
	if probeErr != nil {
		return "", fmt.Errorf("ask %s about sandbox %s: %w", s.cfg.Provider.Name(), sb.ID, probeErr)
	}

	state, err := reconciled(sb, status)
	if err != nil {
		return "", fmt.Errorf("check the snapshot of sandbox %s: %w", sb.ID, err)
	}
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

	// A pending record whose start never took is a create the daemon dropped: it ends failed, not stopped.
	if state == models.StateFailed {
		err = s.cfg.Repo.Update(sb.ID, func(rec *models.Sandbox) error {
			rec.State = models.StateFailed
			rec.PID = 0
			rec.FailedReason = InterruptedReason

			return nil
		})
		if err != nil {
			return "", fmt.Errorf("sandbox %s never finished its create but its record was not updated: %w", sb.ID, err)
		}
		report(fmt.Sprintf("sandbox %s said %s and nothing runs behind it: the record now says failed, %s", sb.ID, sb.State, InterruptedReason))

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
func reconciled(sb models.Sandbox, status models.Status) (models.State, error) {
	if status.Alive() {
		return models.StateRunning, nil
	}

	if sb.State == models.StatePaused {
		held, err := hasCheckpoint(sb.Snapshot)
		if err != nil {
			return "", err
		}
		if held {
			return models.StatePaused, nil
		}
	}

	// A pending create whose start never took never reached running, so it is a failed create.
	if sb.State == models.StatePending {
		return models.StateFailed, nil
	}

	if sb.State == models.StateRunning || sb.State == models.StatePaused {
		return models.StateStopped, nil
	}

	// A created record never ran, and a stopped one is already right.
	return sb.State, nil
}

// hasCheckpoint answers only what it read. A stat that failed for any other reason is not an absence.
func hasCheckpoint(dir string) (bool, error) {
	if dir == "" {
		return false, nil
	}

	path := filepath.Join(dir, checkpointFile)
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat the checkpoint %s: %w", path, err)
	}

	return true, nil
}
