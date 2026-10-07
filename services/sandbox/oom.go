package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// OOMHealthyRun is the run that starts the backoff over: minutes, since gVisor takes half a minute to die of its memory (SHARD-188).
const OOMHealthyRun = 10 * time.Minute

// OOMRestartBackoff is the wait before the second start again in a row; it doubles after each one, up to OOMRestartBackoffCap.
const OOMRestartBackoff = 10 * time.Second

// OOMRestartBackoffCap is the longest wait, so a sandbox that runs out of memory at boot starts again every five minutes.
const OOMRestartBackoffCap = 5 * time.Minute

// recordOOMKilled stops the record with the reason and schedules the start again, which the liveness tick runs once due.
func (s *Service) recordOOMKilled(id string, sb models.Sandbox, now time.Time, report func(string)) error {
	oom := nextOOM(sb, now)
	err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.State = models.StateStopped
		rec.PID = 0
		rec.StoppedReason = OOMKilledReason
		rec.UnresponsiveReason = ""
		rec.OOM = &oom

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s %s but its record was not updated: %w", id, OOMKilledReason, err)
	}
	s.dropExecs(id)
	report(fmt.Sprintf("sandbox %s: %s, the record now says stopped and the daemon starts it again %s (%s)", id, OOMKilledReason, when(oom.RestartAt, now), kills(oom)))

	return nil
}

// nextOOM counts one more kill, and starts the backoff over when the run that ended outlasted OOMHealthyRun.
func nextOOM(sb models.Sandbox, now time.Time) models.OOM {
	var oom models.OOM
	if sb.OOM != nil {
		oom = *sb.OOM
	}
	if sb.RunStartedAt.IsZero() || now.Sub(sb.RunStartedAt) >= OOMHealthyRun {
		oom.InARow = 0
	}
	oom.Kills++
	oom.InARow++
	oom.KilledAt = now
	oom.RestartAt = now.Add(oomBackoff(oom.InARow))

	return oom
}

// oomBackoff is the wait after the given kill in a row: nothing after the first, then doubling up to the cap.
func oomBackoff(inARow int) time.Duration {
	if inARow <= 1 {
		return 0
	}

	wait := OOMRestartBackoff
	for range inARow - 2 {
		wait = min(wait*2, OOMRestartBackoffCap)
	}

	return wait
}

// startAgainWhenDue starts a sandbox an OOM kill stopped, once its backoff has passed and nothing called the start off.
func (s *Service) startAgainWhenDue(ctx context.Context, sb models.Sandbox, now time.Time, report func(string)) error {
	if now.Before(sb.OOM.RestartAt) {
		return nil
	}

	unlock, ok := s.tryLock(sb.ID)
	if !ok {
		return nil
	}
	defer unlock()

	// The list may be a tick old: a stop, a start or an rm that landed since took the start again away.
	current, err := s.cfg.Repo.Get(sb.ID)
	if errors.Is(err, sandboxstate.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.State != models.StateStopped || !current.OOM.RestartDue() || !current.OOM.RestartAt.Equal(sb.OOM.RestartAt) {
		return nil
	}

	if err := s.start(ctx, sb.ID); err != nil {
		return s.retryOOMStart(sb.ID, current.OOM.RestartAt, now, err, report)
	}
	report(fmt.Sprintf("sandbox %s: started again after it ran out of memory (%s)", sb.ID, kills(*current.OOM)))

	return nil
}

// retryOOMStart moves a failed start again one backoff on, so a start that keeps failing never runs every tick.
func (s *Service) retryOOMStart(id string, due, now time.Time, cause error, report func(string)) error {
	var next time.Time
	err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		// The failed start may have found the sandbox running after all, and then no start is owed.
		if rec.State != models.StateStopped || !rec.OOM.RestartDue() || !rec.OOM.RestartAt.Equal(due) {
			return nil
		}
		rec.OOM.InARow++
		rec.OOM.RestartAt = now.Add(oomBackoff(rec.OOM.InARow))
		next = rec.OOM.RestartAt

		return nil
	})
	failed := fmt.Errorf("start sandbox %s again after it ran out of memory: %w", id, cause)
	if err != nil {
		return errors.Join(failed, fmt.Errorf("sandbox %s: put off the next start again: %w", id, err))
	}
	if !next.IsZero() {
		report(fmt.Sprintf("sandbox %s: the start again after it ran out of memory failed, the daemon tries again %s", id, when(next, now)))
	}

	return failed
}

// callOffOOMRestart drops the start again a record waits on, once a stop or another start made it moot.
func callOffOOMRestart(rec *models.Sandbox) {
	if rec.OOM != nil {
		rec.OOM.RestartAt = time.Time{}
	}
}

// kills names the count the way the daemon log reads it.
func kills(oom models.OOM) string {
	return fmt.Sprintf("kill %d, %d in a row", oom.Kills, oom.InARow)
}

// when is a due time as the log reads it: now, or the wait and the clock time.
func when(due, now time.Time) string {
	if !due.After(now) {
		return "now"
	}

	return fmt.Sprintf("in %s, at %s", due.Sub(now).Round(time.Second), due.UTC().Format(time.RFC3339))
}
