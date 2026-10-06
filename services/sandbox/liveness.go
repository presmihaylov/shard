package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/presmihaylov/shard/models"
)

// OOMKilledReason is what a record says once the host ended the sandbox for holding too much memory.
const OOMKilledReason = "ran out of memory and the host ended it"

// DiedReason is what a record says once the liveness task found the sandbox process gone with no stop behind it.
const DiedReason = "the sandbox process died"

// SupervisorFailedReason is what a record says once shard-init itself died, followed by the reason it gave.
const SupervisorFailedReason = "shard-init failed"

// Liveness records each entrypoint exit, and stops a sandbox whose process is gone, with the OOM as its reason if there was one.
func (s *Service) Liveness(ctx context.Context, sandboxes []models.Sandbox, report func(string)) error {
	var errs []error
	for _, sb := range sandboxes {
		if !sb.State.Live() {
			continue
		}
		if err := s.reconcileLive(ctx, sb, report); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// reconcileLive probes the substrate without the lock, because Status can wedge and a stop on this or any
// other sandbox must not wait on it. It locks to write and to probe a marked run again, and bails if the run changed.
func (s *Service) reconcileLive(ctx context.Context, sb models.Sandbox, report func(string)) error {
	// The list may be a tick old: a stop that landed since means this sandbox never needs the substrate.
	if before, err := s.cfg.Repo.Get(sb.ID); err != nil || !before.State.Live() || before.PID != sb.PID || !before.RunStartedAt.Equal(sb.RunStartedAt) {
		return err
	}

	status, ok, err := s.probeLive(ctx, sb.ID, report)
	if !ok {
		return err
	}

	unlock, ok := s.tryLock(sb.ID)
	if !ok {
		return nil
	}
	defer unlock()

	// A stop, or a stop and a start that even reused the PID, landed while the probe ran: RunStartedAt catches it.
	current, err := s.cfg.Repo.Get(sb.ID)
	if err != nil {
		return err
	}
	if !current.State.Live() || current.PID != sb.PID || !current.RunStartedAt.Equal(sb.RunStartedAt) {
		return nil
	}
	// A silent substrate process may still answer, so it is marked and never ended here; only stop ends it (SHARD-421).
	if status.State == models.StateUnresponsive {
		return s.recordUnresponsive(sb.ID, current, status.Reason, report)
	}
	if status.Alive() && current.State == models.StateUnresponsive {
		if err := s.recordAnswered(sb.ID, status, report); err != nil {
			return err
		}
		current, err = s.cfg.Repo.Get(sb.ID)
		if err != nil {
			return err
		}
	}

	// A pause can commit between the probe and the lock, so only a probe under the lock may drop its mark (SHARD-429).
	if ranPast(current, status) {
		status, ok, err = s.probeLive(ctx, sb.ID, report)
		if !ok {
			return err
		}
	}

	// A pause that could not reconcile itself left its mark over the checkpoint it wrote, and maybe a frozen sandbox (SHARD-366).
	dir, err := s.cutPause(ctx, current, status)
	if err != nil {
		return err
	}
	if dir != "" {
		return s.recordCutPause(sb.ID, current.State, dir, report)
	}
	if ranPast(current, status) {
		if err := s.dropMark(sb.ID, report); err != nil {
			return err
		}
	}

	// The sandbox outlives its entrypoint, so a live one that lost its entrypoint stays running with the exit noted.
	if status.Alive() {
		return s.recordEntrypointExit(ctx, sb.ID, current, report)
	}
	if status.OOMKilled {
		return s.recordDied(sb.ID, OOMKilledReason, report)
	}
	if status.SupervisorFailed != "" {
		return s.recordSupervisorFailed(sb.ID, status.SupervisorFailed, report)
	}

	return s.recordDied(sb.ID, DiedReason, report)
}

// probeLive asks the substrate about a running sandbox; false with no error is a probe out of budget, which it reported.
func (s *Service) probeLive(ctx context.Context, id string, report func(string)) (models.Status, bool, error) {
	status, err := s.status(ctx, id, "liveness")
	if timeout, ok := errors.AsType[*SubstrateTimeoutError](err); ok {
		report(fmt.Sprintf("sandbox %s: the substrate did not answer within %s, the record is left as it is and the next tick asks again", id, timeout.Budget))

		return models.Status{}, false, nil
	}
	if err != nil {
		return models.Status{}, false, fmt.Errorf("ask %s about sandbox %s: %w", s.cfg.Provider.Name(), id, err)
	}

	return status, true, nil
}

// recordEntrypointExit writes the entrypoint's exit onto a still-running record, so ls tells a crash from a
// clean exit and the sandbox stays up for another exec. It is idempotent: a recorded exit is left alone.
func (s *Service) recordEntrypointExit(ctx context.Context, id string, sb models.Sandbox, report func(string)) error {
	exit, err := s.cfg.Provider.ExitStatus(ctx, id)
	// Log and continue, as Pres decided on 2026-10-03: a failed task backs off liveness for every sandbox, and only this guest loses its own exit.
	if errors.Is(err, models.ErrExitFileTooLarge) {
		report(fmt.Sprintf("sandbox %s: %v; its entrypoint exit is unknown until the next one", id, err))
		return nil
	}
	if errors.Is(err, models.ErrExitChannelReplaced) {
		return s.recordExitChannel(id, sb, err.Error(), report)
	}
	if err != nil {
		return fmt.Errorf("read the exit of sandbox %s: %w", id, err)
	}
	if err := s.recordExitChannel(id, sb, "", report); err != nil {
		return err
	}
	if exit == nil {
		return nil
	}
	if sb.ExitStatus != nil && *sb.ExitStatus == *exit {
		return nil
	}

	err = s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.ExitStatus = exit

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s entrypoint exited but its record was not updated: %w", id, err)
	}
	report(fmt.Sprintf("sandbox %s entrypoint exited (code %d, signal %d): recorded, the sandbox stays running", id, exit.Code, exit.Signal))

	return nil
}

// noteUnresponsive records what a verb's own probe found, unless a verb holds the sandbox or its run changed since; the next tick asks then.
func (s *Service) noteUnresponsive(id string, seen models.Sandbox, reason string) error {
	unlock, ok := s.tryLock(id)
	if !ok {
		return nil
	}
	defer unlock()

	current, err := s.cfg.Repo.Get(id)
	if err != nil {
		return err
	}
	if !current.State.Live() || current.PID != seen.PID || !current.RunStartedAt.Equal(seen.RunStartedAt) {
		return nil
	}

	return s.recordUnresponsive(id, current, reason, s.report)
}

// recordUnresponsive marks a live record whose substrate process missed its probe bound, and keeps its pid and its run.
func (s *Service) recordUnresponsive(id string, sb models.Sandbox, reason string, report func(string)) error {
	if sb.State == models.StateUnresponsive && sb.UnresponsiveReason == reason {
		return nil
	}
	err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.State = models.StateUnresponsive
		rec.UnresponsiveReason = reason

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s did not answer but its record was not updated: %w", id, err)
	}
	report(fmt.Sprintf("sandbox %s: %s, the record now says unresponsive until it answers or a stop ends it", id, reason))

	return nil
}

// recordAnswered makes an unresponsive record running again once its substrate process answers, with its run kept.
func (s *Service) recordAnswered(id string, status models.Status, report func(string)) error {
	dropped := false
	err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.State = models.StateRunning
		rec.UnresponsiveReason = ""
		// A process that runs again ran past any pause it held, so its old checkpoint must never become the pause (SHARD-442).
		if status.State == models.StateRunning && rec.Pausing {
			rec.Pausing = false
			dropped = true
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s answers again but its record was not updated: %w", id, err)
	}
	if dropped {
		report(fmt.Sprintf("sandbox %s answers again and runs on past a pause the daemon never recorded: the record now says running and drops the pause mark", id))

		return nil
	}
	report(fmt.Sprintf("sandbox %s answers again, the record now says running", id))

	return nil
}

// recordExitChannel keeps on the record why the exit cannot be read, so inspect names it, and reports each change once.
func (s *Service) recordExitChannel(id string, sb models.Sandbox, why string, report func(string)) error {
	if sb.ExitChannel == why {
		return nil
	}

	err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.ExitChannel = why

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s: record its exit channel: %w", id, err)
	}
	if why == "" {
		report(fmt.Sprintf("sandbox %s: its exit channel reads again", id))
		return nil
	}
	report(fmt.Sprintf("sandbox %s: %s; its entrypoint exit is unknown until the channel reads again", id, why))

	return nil
}

// recordDied stops the record of a sandbox that died with no stop behind it, so start can bring it back over its disk.
func (s *Service) recordDied(id, reason string, report func(string)) error {
	err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.State = models.StateStopped
		rec.PID = 0
		rec.StoppedReason = reason
		rec.UnresponsiveReason = ""

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s is gone but its record was not updated: %w", id, err)
	}
	s.dropExecs(id)
	report(fmt.Sprintf("sandbox %s: %s, the record now says stopped", id, reason))

	return nil
}

// recordSupervisorFailed stops the record of a sandbox whose shard-init died, with its exit and the reason it gave.
func (s *Service) recordSupervisorFailed(id, why string, report func(string)) error {
	err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.State = models.StateStopped
		rec.PID = 0
		rec.UnresponsiveReason = ""
		supervisorFailed(rec, why)

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s: %s, but its record was not updated: %w", id, SupervisorFailedReason, err)
	}
	report(fmt.Sprintf("sandbox %s: %s: %s, the record now says stopped", id, SupervisorFailedReason, why))

	return nil
}

// supervisorFailed makes shard-init's death the record's exit, as runsc wait reads its 125 on gVisor.
func supervisorFailed(rec *models.Sandbox, why string) {
	rec.StoppedReason = SupervisorFailedReason + ": " + why
	rec.ExitStatus = &models.ExitStatus{Code: models.SupervisorFailedExitCode}
}
