package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/presmihaylov/shard/models"
)

// OOMHealthyRun is how long a sandbox must run after a start before the next OOM resets its count (Docker's number).
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

	unlock := s.lock(sb.ID)
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
		return s.recordEntrypointExit(ctx, sb.ID, current, report)
	}
	if status.OOMKilled {
		return s.handleOOMKilled(ctx, sb.ID, current, now, report)
	}

	return s.recordDied(sb.ID, report)
}

// recordEntrypointExit writes the entrypoint's exit onto a still-running record, so ls tells a crash from a
// clean exit and the sandbox stays up for another exec. It is idempotent: a recorded exit is left alone.
func (s *Service) recordEntrypointExit(ctx context.Context, id string, sb models.Sandbox, report func(string)) error {
	exit, err := s.cfg.Provider.ExitStatus(ctx, id)
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
	report(fmt.Sprintf("sandbox %s: %s, the record now says stopped", id, DiedReason))

	return nil
}

// handleOOMKilled runs the memory decision: start the sandbox again when its record asks and the limit allows,
// else stop it with the reason. The record counts the start before the run, so one that fails leaves no loop.
func (s *Service) handleOOMKilled(ctx context.Context, id string, sb models.Sandbox, now time.Time, report func(string)) error {
	// A run that lasted the healthy window since its last start begins the count over, so a rare OOM never spends the limit.
	restarts := sb.OOMRestarts
	if !sb.StartedAt.IsZero() && now.Sub(sb.StartedAt) >= OOMHealthyRun {
		restarts = 0
	}

	restart := sb.RestartOnOOM && (sb.MaxOOMRestarts == 0 || restarts < sb.MaxOOMRestarts)
	reason := OOMKilledReason
	if sb.RestartOnOOM && !restart {
		reason = fmt.Sprintf("%s; the %d starts again the limit allows are spent", OOMKilledReason, sb.MaxOOMRestarts)
	}
	// A sandbox that dies right after every start would otherwise come back on every tick until the limit.
	if restart && now.Before(sb.OOMRestartedAt.Add(oomBackoff(restarts))) {
		return nil
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
		return fmt.Errorf("start sandbox %s again after it %s: %w", id, OOMKilledReason, err)
	}
	report(oomRestartReport(id, restarts+1, sb.MaxOOMRestarts))

	return nil
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
