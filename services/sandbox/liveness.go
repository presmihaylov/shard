package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/presmihaylov/shard/models"
)

// OOMHealthyRun is how long a sandbox must run under its memory throttle before the next OOM resets its count (Docker's number).
const OOMHealthyRun = 10 * time.Second

// OOMRestartBackoff is the wait before the second start again; it doubles after each one, up to a minute.
const OOMRestartBackoff = time.Second

// OOMKilledReason is what a record says once the host ended the sandbox for holding too much memory.
const OOMKilledReason = "ran out of memory and the host ended it"

// DiedReason is what a record says once the liveness task found the sandbox process gone with no stop behind it.
const DiedReason = "the sandbox process died"

// Liveness makes each running record agree with the substrate every tick: it records an entrypoint exit,
// stops a sandbox whose process is gone, and starts an OOM-killed one again when its record asks.
func (s *Service) Liveness(ctx context.Context, sandboxes []models.Sandbox, now time.Time, report func(string)) error {
	var errs []error
	for _, sb := range sandboxes {
		if sb.State != models.StateRunning {
			continue
		}
		if err := s.reconcileLive(ctx, sb, now, report); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// reconcileLive probes the substrate without the lock, because Status can wedge and a stop on this or any
// other sandbox must not wait on it. It takes the lock only to write, and bails if the run has since changed.
func (s *Service) reconcileLive(ctx context.Context, sb models.Sandbox, now time.Time, report func(string)) error {
	// The list may be a tick old: a stop that landed since means this sandbox never needs the substrate.
	if before, err := s.cfg.Repo.Get(sb.ID); err != nil || before.State != models.StateRunning || before.PID != sb.PID || !before.StartedAt.Equal(sb.StartedAt) {
		return err
	}

	status, err := s.status(ctx, sb.ID, "liveness")
	var timeout *SubstrateTimeoutError
	if errors.As(err, &timeout) {
		report(fmt.Sprintf("sandbox %s: the substrate did not answer within %s, the record is left as it is and the next tick asks again", sb.ID, timeout.Budget))
		return nil
	}
	if err != nil {
		return fmt.Errorf("ask %s about sandbox %s: %w", s.cfg.Provider.Name(), sb.ID, err)
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
	if current.State != models.StateRunning || current.PID != sb.PID || !current.StartedAt.Equal(sb.StartedAt) {
		return nil
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
		if restarts, restart := oomRestarts(current, status.Throttles, now); restart && now.Before(current.OOMRestartedAt.Add(oomBackoff(restarts))) {
			return nil
		}

		return s.handleOOMKilled(ctx, sb.ID, current, status.Throttles, now, report)
	}

	return s.recordDied(sb.ID, report)
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
	if err != nil {
		return fmt.Errorf("read the exit of sandbox %s: %w", id, err)
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

// recordDied stops the record of a sandbox whose process is gone with no OOM and no stop behind it, so
// exec reads the truth and start can bring it back.
func (s *Service) recordDied(id string, report func(string)) error {
	err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.State = models.StateStopped
		rec.PID = 0
		rec.StoppedReason = DiedReason

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s is gone but its record was not updated: %w", id, err)
	}
	s.dropExecs(id)
	report(fmt.Sprintf("sandbox %s: %s, the record now says stopped", id, DiedReason))

	return nil
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

	if err := s.start(ctx, id); err != nil {
		var timeout *SubstrateTimeoutError
		if errors.As(err, &timeout) {
			// A wedged runtime must not pin the serial task; the record stays stopped with the start counted.
			report(fmt.Sprintf("sandbox %s %s: the substrate did not answer within %s while starting it again, the record stays stopped", id, OOMKilledReason, timeout.Budget))

			return nil
		}

		return fmt.Errorf("start sandbox %s again after it %s: %w", id, OOMKilledReason, err)
	}
	report(oomRestartReport(id, restarts+1, sb.MaxOOMRestarts))

	return nil
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
