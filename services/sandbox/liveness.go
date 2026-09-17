package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/presmihaylov/shard/models"
)

// OOMRestartCap is how many times the daemon starts one sandbox again after the host ended it for its memory.
const OOMRestartCap = 5

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
		if err := s.reconcileLive(ctx, sb.ID, now, report); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// reconcileLive reads the record again under the lock, because a stop may have landed since the list.
func (s *Service) reconcileLive(ctx context.Context, id string, now time.Time, report func(string)) error {
	unlock := s.lock(id)
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return err
	}
	if sb.State != models.StateRunning {
		return nil
	}

	status, err := s.cfg.Provider.Status(ctx, id)
	if err != nil {
		return fmt.Errorf("ask %s about sandbox %s: %w", s.cfg.Provider.Name(), id, err)
	}

	// The sandbox outlives its entrypoint, so a live one that lost its entrypoint stays running with the exit noted.
	if status.Alive() {
		return s.recordEntrypointExit(ctx, id, sb, report)
	}
	if status.OOMKilled {
		return s.handleOOMKilled(ctx, id, sb, now, report)
	}

	return s.recordDied(id, report)
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

// handleOOMKilled runs the memory decision: start the sandbox again when its record asks and the cap allows,
// else stop it with the reason. The record counts the start before the run, so one that fails leaves no loop.
func (s *Service) handleOOMKilled(ctx context.Context, id string, sb models.Sandbox, now time.Time, report func(string)) error {
	restart := sb.RestartOnOOM && sb.OOMRestarts < OOMRestartCap
	reason := OOMKilledReason
	if sb.RestartOnOOM && !restart {
		reason = fmt.Sprintf("%s; the %d starts again the cap allows are spent", OOMKilledReason, OOMRestartCap)
	}
	// A sandbox that dies right after every start would otherwise come back on every tick until the cap.
	if restart && now.Before(sb.OOMRestartedAt.Add(oomBackoff(sb.OOMRestarts))) {
		return nil
	}

	err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.State = models.StateStopped
		rec.PID = 0
		rec.StoppedReason = reason
		if restart {
			rec.OOMRestarts++
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
	report(fmt.Sprintf("sandbox %s %s: started again, %d of %d", id, OOMKilledReason, sb.OOMRestarts+1, OOMRestartCap))

	return nil
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
