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

// RestartOOMKilled starts again every running record the host ended for its memory that asked, one report line each.
func (s *Service) RestartOOMKilled(ctx context.Context, sandboxes []models.Sandbox, now time.Time, report func(string)) error {
	var errs []error
	for _, sb := range sandboxes {
		if sb.State != models.StateRunning {
			continue
		}
		if err := s.restartIfOOMKilled(ctx, sb.ID, now, report); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// restartIfOOMKilled reads the record again under the lock, because a stop may have landed since the list.
func (s *Service) restartIfOOMKilled(ctx context.Context, id string, now time.Time, report func(string)) error {
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
	if status.Alive() || !status.OOMKilled {
		return nil
	}

	restart := sb.RestartOnOOM && sb.OOMRestarts < OOMRestartCap
	reason := OOMKilledReason
	if sb.RestartOnOOM && !restart {
		reason = fmt.Sprintf("%s; the %d starts again the cap allows are spent", OOMKilledReason, OOMRestartCap)
	}
	// A sandbox that dies right after every start would otherwise come back on every tick until the cap.
	if restart && now.Before(sb.OOMRestartedAt.Add(oomBackoff(sb.OOMRestarts))) {
		return nil
	}

	// The record counts the start before the run, so one that fails leaves the count true and no retry loop.
	err = s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
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
