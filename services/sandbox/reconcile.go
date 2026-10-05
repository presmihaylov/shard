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

// LostReason is what a record says once the daemon found no process and no checkpoint behind it.
const LostReason = "daemon restarted and found no process"

// InterruptedReason is what a pending create's record says once the daemon restarted before it finished.
const InterruptedReason = "the daemon restarted before the create finished"

// DroppedCopyReason is what a fork's record says once the daemon restarted before the copy reached running.
const DroppedCopyReason = "the daemon restarted before the fork finished"

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
		// A cut pause leaves a staged checkpoint, settled once here so a retried record write never reports it twice (SHARD-428).
		err := s.adoptStaging(sb.ID, report)
		if err == nil {
			err = retry("the record of sandbox "+sb.ID, func() error {
				var err error
				state, err = s.applyReconcile(ctx, sb, probes[i].status, probes[i].err, report)

				return err
			})
		}
		if err != nil {
			// A daemon that refused to start could not stop or remove this sandbox, nor serve the others (SHARD-341).
			report(fmt.Sprintf("sandbox %s: %v, the record is left as it is", sb.ID, err))
			state = sb.State
			// A live sandbox whose record write failed still needs its host rules back.
			if probes[i].err == nil && probes[i].status.Alive() {
				state = models.StateRunning
			}
		}
		if state.Live() {
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
	// The host ended it for its memory while the daemon was down, so no verb reads a running record with no process (SHARD-311).
	if sb.State.Live() && !status.Alive() && status.OOMKilled {
		if err := s.recordDied(sb.ID, OOMKilledReason, report); err != nil {
			return "", err
		}

		return models.StateStopped, nil
	}

	// The daemon stopped after a pause installed its checkpoint and before the pause wrote the record (SHARD-366).
	cut, err := s.cutPause(ctx, sb, status)
	if err != nil {
		return "", err
	}
	if cut != "" {
		if err := s.recordCutPause(sb.ID, sb.State, cut, report); err != nil {
			return "", err
		}

		return models.StatePaused, nil
	}

	// A run the substrate carried on past a cut pause holds no pause, and the mark over its checkpoint would make its death one (SHARD-429).
	if ranPast(sb, status) {
		if err := s.dropMark(sb.ID, report); err != nil {
			return "", err
		}
	}

	state, err := reconciled(sb, status)
	if err != nil {
		return "", fmt.Errorf("check the checkpoint of sandbox %s: %w", sb.ID, err)
	}
	if state == sb.State {
		return state, nil
	}

	if state == models.StateUnresponsive {
		if err := s.recordUnresponsive(sb.ID, sb, status.Reason, report); err != nil {
			return "", err
		}

		return state, nil
	}
	// An unresponsive record whose process answers again keeps its run, so it is no fresh start (SHARD-421).
	if state == models.StateRunning && sb.State == models.StateUnresponsive {
		if err := s.recordAnswered(sb.ID, status, report); err != nil {
			return "", err
		}

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

	// A record that never reached running is a create or a fork the daemon dropped: it ends failed, not stopped.
	if state == models.StateFailed {
		if err := s.failDropped(ctx, sb, status, report); err != nil {
			return "", err
		}

		return state, nil
	}

	// A restart that adopted the substrate and read shard-init's own death keeps that reason, as the liveness tick does (SHARD-610).
	if status.SupervisorFailed != "" {
		if err := s.recordSupervisorFailed(sb.ID, status.SupervisorFailed, report); err != nil {
			return "", err
		}

		return models.StateStopped, nil
	}

	err = s.cfg.Repo.Update(sb.ID, func(rec *models.Sandbox) error {
		rec.State = models.StateStopped
		rec.PID = 0
		rec.StoppedReason = LostReason
		rec.UnresponsiveReason = ""

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("sandbox %s is gone but its record was not updated: %w", sb.ID, err)
	}
	report(fmt.Sprintf("sandbox %s said %s and nothing runs behind it: the record now says stopped, %s", sb.ID, sb.State, LostReason))

	return state, nil
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
			return fmt.Errorf("stop sandbox %s, a fork the daemon dropped: %w", sb.ID, err)
		}
		if _, err := s.awaitStopped(ctx, sb.ID); err != nil {
			return err
		}
	}
	// A restore the daemon left behind can run on where Status cannot see it, and a failed record stays until rm.
	if sb.State == models.StateCreated {
		if err := s.cfg.Provider.Remove(ctx, sb.ID); err != nil {
			return fmt.Errorf("tear down sandbox %s, a fork the daemon dropped: %w", sb.ID, err)
		}
	}

	err := s.cfg.Repo.Update(sb.ID, func(rec *models.Sandbox) error {
		rec.State = models.StateFailed
		rec.PID = 0
		// The reason is one of shard's own constants, so the public text is the whole of it.
		rec.FailedReason = reason
		rec.FailedPublic = reason

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

// reconciled trusts the checkpoint over the substrate for a paused sandbox, since a checkpoint holds no process yet still resumes.
func reconciled(sb models.Sandbox, status models.Status) (models.State, error) {
	// No verb rests in created, so it is a fork that never answered: its caller holds an error, not the id.
	if sb.State == models.StateCreated {
		return models.StateFailed, nil
	}

	if status.State == models.StateUnresponsive {
		return models.StateUnresponsive, nil
	}
	// A substrate still reporting paused held a cut pause: keep that truth, or inspect and exec lie (SHARD-411).
	if status.State == models.StatePaused {
		return models.StatePaused, nil
	}

	if status.Alive() {
		return models.StateRunning, nil
	}

	if sb.State == models.StatePaused {
		held, err := hasCheckpoint(sb.Checkpoint)
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

	if sb.State.Live() || sb.State == models.StatePaused {
		return models.StateStopped, nil
	}

	// A stopped record is already right.
	return sb.State, nil
}

// releaser ends a sandbox a cut pause left frozen beside its checkpoint, with no thaw that would run the guest past it.
type releaser interface {
	Release(ctx context.Context, id, dir string) error
}

// cutPause is the checkpoint a marked pause completed and never recorded, after it releases what that pause left behind; empty for none.
func (s *Service) cutPause(ctx context.Context, sb models.Sandbox, status models.Status) (string, error) {
	dir, err := s.markedCheckpoint(sb)
	if err != nil {
		return "", err
	}
	if dir == "" {
		return "", nil
	}
	r, ok := s.cfg.Provider.(releaser)
	if !ok && !status.Alive() {
		return dir, nil
	}
	// A substrate that cannot release keeps the record, rather than call paused what it still holds.
	if !ok || status.Alive() && status.State != models.StatePaused {
		return "", nil
	}
	// A cut after the delete still leaves the merged view mounted, and only the release frees it (SHARD-366).
	if err := r.Release(ctx, sb.ID, dir); err != nil {
		return "", fmt.Errorf("release sandbox %s, which a cut pause left beside its checkpoint: %w", sb.ID, err)
	}

	return dir, nil
}

// markedCheckpoint is the complete checkpoint a marked pause installed for a record still live, answering or not; empty for none.
func (s *Service) markedCheckpoint(sb models.Sandbox) (string, error) {
	// A daemon cut after the checkpoint can leave the mark over a silent shim, and its death must still find the pause (SHARD-442).
	if !sb.State.Live() || !sb.Pausing {
		return "", nil
	}

	dir, err := s.cfg.Repo.CheckpointDir(sb.ID)
	if err != nil {
		return "", fmt.Errorf("check the checkpoint of sandbox %s: %w", sb.ID, err)
	}
	held, err := hasCheckpoint(dir)
	if err != nil {
		return "", fmt.Errorf("check the checkpoint of sandbox %s: %w", sb.ID, err)
	}
	if !held {
		return "", nil
	}

	return dir, nil
}

// ranPast is a marked record the substrate says runs; a frozen or unresponsive one proves no run past the pause.
func ranPast(sb models.Sandbox, status models.Status) bool {
	return sb.State == models.StateRunning && sb.Pausing && status.Alive() && status.State == models.StateRunning
}

// dropMark clears the mark of a pause the substrate ran on past, and reports the correction.
func (s *Service) dropMark(id string, report func(string)) error {
	err := s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.Pausing = false

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s runs on past a cut pause but its record was not updated: %w", id, err)
	}
	report(fmt.Sprintf("sandbox %s said running and the substrate runs it on past a pause the daemon never recorded: the record drops the pause mark", id))

	return nil
}

// recordCutPause records the pause a cut pause completed on the host, and reports the correction.
func (s *Service) recordCutPause(id string, was models.State, dir string, report func(string)) error {
	if err := s.recordPaused(id, dir); err != nil {
		return err
	}
	report(fmt.Sprintf("sandbox %s said %s and a pause the daemon never recorded left a complete checkpoint: the record now says paused", id, was))

	return nil
}

// adoptStaging hands the provider the staging a cut pause left, which it finishes or drops, and says whether it kept or removed it (SHARD-428).
func (s *Service) adoptStaging(id string, report func(string)) error {
	dir, err := s.cfg.Repo.CheckpointDir(id)
	if err != nil {
		return fmt.Errorf("find the checkpoint staging of sandbox %s: %w", id, err)
	}
	staging := dir + ".tmp"
	held, err := exists(staging)
	if err != nil {
		return fmt.Errorf("check the checkpoint staging of sandbox %s: %w", id, err)
	}
	if err := s.cfg.Provider.AdoptStaging(dir); err != nil {
		return fmt.Errorf("adopt the checkpoint staging of sandbox %s: %w", id, err)
	}
	if !held {
		return nil
	}

	kept, err := exists(staging)
	if err != nil {
		return fmt.Errorf("check the checkpoint staging of sandbox %s: %w", id, err)
	}
	if kept {
		report(fmt.Sprintf("sandbox %s: %s kept the checkpoint staging %s a cut pause left", id, s.cfg.Provider.Name(), staging))

		return nil
	}
	report(fmt.Sprintf("sandbox %s: %s removed the checkpoint staging %s a cut pause left", id, s.cfg.Provider.Name(), staging))

	return nil
}

func hasCheckpoint(dir string) (bool, error) {
	if dir == "" {
		return false, nil
	}

	return exists(filepath.Join(dir, checkpointFile))
}

// exists answers only what it read. A stat that failed for any other reason is not an absence.
func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}

	return true, nil
}
