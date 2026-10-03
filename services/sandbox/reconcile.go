package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
)

// LostReason is what a record says once the daemon found no process and no snapshot behind it.
const LostReason = "daemon restarted and found no process"

// InterruptedReason is what a pending create's record says once the daemon restarted before it finished.
const InterruptedReason = "the daemon restarted before the create finished"

// DroppedCopyReason is what a fork or clone's record says once the daemon restarted before the copy reached running.
const DroppedCopyReason = "the daemon restarted before the fork or clone finished"

// ReconcileConcurrency bounds the startup probes in flight, so N frozen sandboxes cost about one budget, not N.
const ReconcileConcurrency = 16

// ReconcileAll makes the records agree with the substrate, before the daemon serves its first verb.
// It corrects a record and never deletes one, and it reports one line per record it corrected or could not check.
func (s *Service) ReconcileAll(ctx context.Context, sandboxes []models.Sandbox, report func(string), retry func(step string, run func() error) error) error {
	// The probe is the slow part, so run every probe concurrently, then apply the corrections one at a time.
	probes := s.probeAll(ctx, sandboxes)

	running := 0
	for i, sb := range sandboxes {
		var state models.State
		err := retry("the record of sandbox "+sb.ID, func() error {
			var err error
			state, err = s.applyReconcile(ctx, sb, probes[i].status, probes[i].err, report)

			return err
		})
		if err != nil {
			// A daemon that refused to start could not stop or remove this sandbox, nor serve the others (SHARD-341).
			report(fmt.Sprintf("sandbox %s: %v, the record is left as it is", sb.ID, err))
			state = sb.State
			// A live sandbox whose record write failed still needs its host rules back.
			if probes[i].err == nil && probes[i].status.Alive() {
				state = models.StateRunning
			}
		}
		if state == models.StateRunning {
			running++
		}
	}

	// Host netfilter is the policy of record, and nothing re-applied it while the last daemon was down.
	if running > 0 {
		if err := s.cfg.Network.ReapplyAll(ctx); err != nil {
			return fmt.Errorf("re-apply the host rules for %d running sandboxes: %w", running, err)
		}
	}

	return nil
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
	if sb.State == models.StateRunning && !status.Alive() && status.OOMKilled {
		return s.reconcileOOMKilled(ctx, sb, status, report)
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

	// A cut pause the record never recorded: the substrate holds it paused, so the record catches up.
	if state == models.StatePaused {
		err = s.cfg.Repo.Update(sb.ID, func(rec *models.Sandbox) error {
			rec.State = models.StatePaused
			rec.PID = 0

			return nil
		})
		if err != nil {
			return "", fmt.Errorf("sandbox %s is paused but its record was not updated: %w", sb.ID, err)
		}
		report(fmt.Sprintf("sandbox %s said %s and the substrate holds it paused: the record now says paused", sb.ID, sb.State))

		return state, nil
	}

	// A record that never reached running is a create, fork or clone the daemon dropped: it ends failed, not stopped.
	if state == models.StateFailed {
		if err := s.failDropped(ctx, sb, status, report); err != nil {
			return "", err
		}

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

// reconcileOOMKilled takes the tick's memory decision without the backoff that bounds a loop of ticks, so no verb reads a running record with no process (SHARD-311).
func (s *Service) reconcileOOMKilled(ctx context.Context, sb models.Sandbox, status models.Status, report func(string)) (models.State, error) {
	handled := s.handleOOMKilled(ctx, sb.ID, sb, status.Throttles, time.Now().UTC(), report)
	rec, err := s.cfg.Repo.Get(sb.ID)
	if err != nil {
		return "", errors.Join(handled, fmt.Errorf("read the record of sandbox %s: %w", sb.ID, err))
	}
	// A start again that failed comes after the record took the stop, so it is not left as it is.
	if handled != nil && rec.State != sb.State {
		report(fmt.Sprintf("sandbox %s: %v, the record now says %s", sb.ID, handled, rec.State))

		return rec.State, nil
	}

	return rec.State, handled
}

// failDropped ends the record of a verb the daemon dropped before it answered: it stops a copy that runs on and tears its substrate down.
func (s *Service) failDropped(ctx context.Context, sb models.Sandbox, status models.Status, report func(string)) error {
	reason := InterruptedReason
	if sb.State == models.StateCreated {
		reason = DroppedCopyReason
	}

	// rm refuses a live sandbox and stop refuses a failed one, so a copy left running here could never be removed.
	if status.Alive() {
		if err := s.cfg.Provider.Stop(ctx, sb.ID, 0); err != nil {
			return fmt.Errorf("stop sandbox %s, a fork or clone the daemon dropped: %w", sb.ID, err)
		}
		if err := s.awaitStopped(ctx, sb.ID); err != nil {
			return err
		}
	}
	// A restore the daemon left behind can run on where Status cannot see it, and a failed record stays until rm.
	if sb.State == models.StateCreated {
		if err := s.cfg.Provider.Remove(ctx, sb.ID); err != nil {
			return fmt.Errorf("tear down sandbox %s, a fork or clone the daemon dropped: %w", sb.ID, err)
		}
	}

	err := s.cfg.Repo.Update(sb.ID, func(rec *models.Sandbox) error {
		rec.State = models.StateFailed
		rec.PID = 0
		rec.FailedReason = reason

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s never reached running but its record was not updated: %w", sb.ID, err)
	}

	done := "the record now says failed"
	if sb.State == models.StateCreated {
		done = "its substrate is torn down and the record now says failed"
	}
	if status.Alive() {
		report(fmt.Sprintf("sandbox %s said %s and the substrate held its process %d: it is stopped, %s, %s", sb.ID, sb.State, status.PID, done, reason))

		return nil
	}
	report(fmt.Sprintf("sandbox %s said %s and nothing runs behind it: %s, %s", sb.ID, sb.State, done, reason))

	return nil
}

// reconciled is the state the record should hold: what the substrate says, and for a paused one what
// the snapshot on disk says, because a checkpoint holds no process and resume still brings it back.
func reconciled(sb models.Sandbox, status models.Status) (models.State, error) {
	// No verb rests in created, so it is a fork or clone that never answered: its caller holds an error, not the id.
	if sb.State == models.StateCreated {
		return models.StateFailed, nil
	}

	// A substrate still reporting paused held a cut pause: keep that truth, or inspect and exec lie (SHARD-411).
	if status.State == models.StatePaused {
		return models.StatePaused, nil
	}

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

	// A stopped record is already right.
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
