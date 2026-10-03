package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// OOMHealthyRun is how long a sandbox must run under its memory throttle before the next OOM resets its count (Docker's number).
const OOMHealthyRun = 10 * time.Second

// OOMRestartBackoff is the wait before the second start again; it doubles after each one, up to a minute.
const OOMRestartBackoff = time.Second

// OOMKilledReason is what a record says once the host ended the sandbox for holding too much memory.
const OOMKilledReason = "ran out of memory and the host ended it"

// DiedReason is what a record says once the liveness task found the sandbox process gone with no stop behind it.
const DiedReason = "the sandbox process died"

// SupervisorFailedReason is what a record says once shard-init itself died, followed by the reason it gave.
const SupervisorFailedReason = "shard-init failed"

// Liveness makes each running record agree with the substrate every tick: it records an entrypoint exit,
// stops a sandbox whose process is gone, and starts an OOM-killed one again when its record asks.
func (s *Service) Liveness(ctx context.Context, sandboxes []models.Sandbox, now time.Time, report func(string)) error {
	var errs []error
	for _, sb := range sandboxes {
		var err error
		switch {
		case sb.State.Live():
			err = s.reconcileLive(ctx, sb, now, report)
		case sb.State == models.StateStopped && !sb.OOMRestartDue.IsZero():
			err = s.startAgainWhenDue(ctx, sb, now, report)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// reconcileLive probes the substrate without the lock, because Status can wedge and a stop on this or any
// other sandbox must not wait on it. It locks to write and to probe a marked run again, and bails if the run changed.
func (s *Service) reconcileLive(ctx context.Context, sb models.Sandbox, now time.Time, report func(string)) error {
	// The list may be a tick old: a stop that landed since means this sandbox never needs the substrate.
	if before, err := s.cfg.Repo.Get(sb.ID); err != nil || !before.State.Live() || before.PID != sb.PID || !before.StartedAt.Equal(sb.StartedAt) {
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

	// A stop, or a stop and a start that even reused the PID, landed while the probe ran: StartedAt catches it.
	current, err := s.cfg.Repo.Get(sb.ID)
	if err != nil {
		return err
	}
	if !current.State.Live() || current.PID != sb.PID || !current.StartedAt.Equal(sb.StartedAt) {
		return nil
	}
	// A silent substrate process may still answer, so it is marked and never ended here; only stop ends it (SHARD-421).
	if status.State == models.StateUnresponsive {
		return s.recordUnresponsive(sb.ID, current, status.Reason, report)
	}
	if status.Alive() && current.State == models.StateUnresponsive {
		if err := s.recordAnswered(sb.ID, report); err != nil {
			return err
		}
		current.State = models.StateRunning
		current.UnresponsiveReason = ""
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
		return s.recordCutPause(sb.ID, dir, report)
	}
	if ranPast(current, status) {
		if err := s.dropMark(sb.ID, report); err != nil {
			return err
		}
	}

	// The sandbox outlives its entrypoint, so a live one that lost its entrypoint stays running with the exit noted.
	if status.Alive() {
		if err := s.recordCalm(sb.ID, current, status.Throttles, now); err != nil {
			return err
		}

		return s.recordEntrypointExit(ctx, sb.ID, current, report)
	}
	if status.OOMKilled {
		// The kill ended every exec with the sandbox; drop them before a backoff wait can hold their buffers for a minute.
		s.dropExecs(sb.ID)
		// A sandbox that dies right after every start would otherwise come back on every tick until the limit.
		restarts, restart := oomRestarts(current, status.Throttles, now)
		if due := current.OOMRestartedAt.Add(oomBackoff(restarts)); restart && now.Before(due) {
			return s.waitOOMBackoff(sb.ID, due, report)
		}

		return s.handleOOMKilled(ctx, sb.ID, current, status.Throttles, now, report)
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

// recordCalm keeps the tick's throttle count on the record, and latches a healthy run there, so a daemon restart keeps both.
func (s *Service) recordCalm(id string, sb models.Sandbox, throttles int64, now time.Time) error {
	if !sb.RestartOnOOM {
		return nil
	}

	healthy := healthyRun(sb, throttles, now)
	if throttles == sb.MemoryThrottles && healthy == sb.HealthyRun {
		return nil
	}

	err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.MemoryThrottles = throttles
		rec.HealthyRun = healthy
		if throttles > sb.MemoryThrottles {
			rec.CalmSince = now
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s: record its memory throttle count: %w", id, err)
	}

	return nil
}

// healthyRun reports a run seen OOMHealthyRun in a row under its memory throttle; with no throttle that is the time since the start.
func healthyRun(sb models.Sandbox, throttles int64, now time.Time) bool {
	if sb.HealthyRun {
		return true
	}

	since := sb.CalmSince
	if since.IsZero() {
		since = sb.StartedAt
	}

	return !since.IsZero() && throttles <= sb.MemoryThrottles && now.Sub(since) >= OOMHealthyRun
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
	if !current.State.Live() || current.PID != seen.PID || !current.StartedAt.Equal(seen.StartedAt) {
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
func (s *Service) recordAnswered(id string, report func(string)) error {
	err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.State = models.StateRunning
		rec.UnresponsiveReason = ""

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s answers again but its record was not updated: %w", id, err)
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

// recordDied stops the record of a sandbox whose process is gone with no OOM and no stop behind it, so
// exec reads the truth and start can bring it back.
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

// handleOOMKilled runs the memory decision: start the sandbox again when its record asks and the limit allows,
// else stop it with the reason. The record counts the start before the run, so one that fails leaves no loop.
func (s *Service) handleOOMKilled(ctx context.Context, id string, sb models.Sandbox, throttles int64, now time.Time, report func(string)) error {
	restarts, restart := oomRestarts(sb, throttles, now)
	reason := OOMKilledReason
	if sb.RestartOnOOM && !restart {
		reason = fmt.Sprintf("%s; the %d starts again the limit allows are spent", OOMKilledReason, sb.MaxOOMRestarts)
	}

	err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.State = models.StateStopped
		rec.PID = 0
		rec.StoppedReason = reason
		if restart {
			rec.OOMRestarts = restarts + 1
			rec.OOMRestartedAt = now
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s %s but its record was not updated: %w", id, OOMKilledReason, err)
	}
	if !restart {
		report(fmt.Sprintf("sandbox %s %s: the record now says stopped", id, reason))

		return nil
	}

	return s.startAgain(ctx, id, restarts+1, sb.MaxOOMRestarts, report)
}

// waitOOMBackoff takes the record out of running at the kill, so no verb reads the dead pid while the start again waits (SHARD-425).
func (s *Service) waitOOMBackoff(id string, due time.Time, report func(string)) error {
	reason := fmt.Sprintf("%s; it starts again at %s", OOMKilledReason, due.Format(time.RFC3339))
	err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.State = models.StateStopped
		rec.PID = 0
		rec.StoppedReason = reason
		rec.UnresponsiveReason = ""
		rec.OOMRestartDue = due

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s %s but its record was not updated: %w", id, OOMKilledReason, err)
	}
	report(fmt.Sprintf("sandbox %s %s: the record now says stopped", id, reason))

	return nil
}

// startAgainWhenDue starts a sandbox that waited out its backoff stopped, once the wait has passed.
func (s *Service) startAgainWhenDue(ctx context.Context, sb models.Sandbox, now time.Time, report func(string)) error {
	if now.Before(sb.OOMRestartDue) {
		return nil
	}

	unlock, ok := s.tryLock(sb.ID)
	if !ok {
		return nil
	}
	defer unlock()

	// The list may be a tick old: a stop, a start or an rm that landed since took the wait away.
	current, err := s.cfg.Repo.Get(sb.ID)
	if errors.Is(err, sandboxstate.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.State != models.StateStopped || !current.OOMRestartDue.Equal(sb.OOMRestartDue) {
		return nil
	}

	err = s.cfg.Repo.Update(sb.ID, func(rec *models.Sandbox) error {
		rec.StoppedReason = OOMKilledReason
		rec.OOMRestarts = current.OOMRestarts + 1
		rec.OOMRestartedAt = now
		rec.OOMRestartDue = time.Time{}

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s %s but its start again was not counted: %w", sb.ID, OOMKilledReason, err)
	}

	return s.startAgain(ctx, sb.ID, current.OOMRestarts+1, current.MaxOOMRestarts, report)
}

// startAgain runs a start again the record already counted, so one that fails leaves no loop.
func (s *Service) startAgain(ctx context.Context, id string, count, limit int, report func(string)) error {
	if err := s.start(ctx, id); err != nil {
		var timeout *SubstrateTimeoutError
		if errors.As(err, &timeout) {
			// A wedged runtime must not pin the serial task; the record stays stopped with the start counted.
			report(fmt.Sprintf("sandbox %s %s: the substrate did not answer within %s while starting it again, the record stays stopped", id, OOMKilledReason, timeout.Budget))

			return nil
		}

		return fmt.Errorf("start sandbox %s again after it %s: %w", id, OOMKilledReason, err)
	}
	report(oomRestartReport(id, count, limit))

	return nil
}

// callOffOOMWait drops the start again a stopped record waits on, because an operator stop outranks it.
func callOffOOMWait(rec *models.Sandbox) {
	if rec.OOMRestartDue.IsZero() {
		return
	}
	rec.OOMRestartDue = time.Time{}
	rec.StoppedReason = OOMKilledReason
}

// oomRestarts is the count an OOM now goes on from, and whether the record and its limit allow one more start again.
func oomRestarts(sb models.Sandbox, throttles int64, now time.Time) (int, bool) {
	// A healthy run begins the count over, so a rare OOM never spends the limit and a throttled loop always does.
	restarts := sb.OOMRestarts
	if healthyRun(sb, throttles, now) {
		restarts = 0
	}

	return restarts, sb.RestartOnOOM && (sb.MaxOOMRestarts == 0 || restarts < sb.MaxOOMRestarts)
}

// oomRestartReport names the limit when the sandbox has one and leaves it off when the starts again are unlimited.
func oomRestartReport(id string, count, limit int) string {
	if limit > 0 {
		return fmt.Sprintf("sandbox %s %s: started again, %d of %d", id, OOMKilledReason, count, limit)
	}

	return fmt.Sprintf("sandbox %s %s: started again, %d", id, OOMKilledReason, count)
}

// oomBackoff is the wait after the given number of starts again: nothing before the first, then doubling.
func oomBackoff(restarts int) time.Duration {
	if restarts == 0 {
		return 0
	}

	wait := OOMRestartBackoff
	for range restarts - 1 {
		wait = min(wait*2, time.Minute)
	}

	return wait
}
