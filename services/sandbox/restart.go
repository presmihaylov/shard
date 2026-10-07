package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/presmihaylov/shard/models"
)

// DefaultRestartBackoff is the first wait a policy that names none gets; retries default to unlimited.
const DefaultRestartBackoff = 1

// validRestart refuses a policy that is not one of the four, and a setting the policy never reads.
func validRestart(r models.RestartSpec) error {
	switch r.Policy {
	case models.RestartNo, models.RestartOnFailure, models.RestartAlways, models.RestartUnlessStopped:
	default:
		return fmt.Errorf("restart.policy is no, on-failure, always or unless-stopped, got %q", r.Policy)
	}
	if r.Retries < 0 {
		return errors.New("restart.retries must be a nonnegative count")
	}
	if r.Backoff < 0 {
		return errors.New("restart.backoff must be nonnegative seconds")
	}
	if r.Backoff > models.RestartBackoffCap {
		return fmt.Errorf("restart.backoff is in seconds and never grows past %d, got %d", models.RestartBackoffCap, r.Backoff)
	}
	if r.Retries != 0 && r.Policy != models.RestartOnFailure {
		return fmt.Errorf("restart.retries is only for on-failure, the one policy that gives up, and the request names %s", r.Policy)
	}
	if !r.Set() && r.Backoff != 0 {
		return errors.New("restart.backoff needs a policy that starts again, and no never does")
	}

	return nil
}

// restartOf fills what the request left at zero, and is unless-stopped when it names no policy.
func restartOf(r *models.RestartSpec) (models.RestartSpec, error) {
	if r == nil {
		return models.RestartSpec{Policy: models.RestartUnlessStopped, Backoff: DefaultRestartBackoff}, nil
	}
	if err := validRestart(*r); err != nil {
		return models.RestartSpec{}, err
	}

	filled := *r
	if filled.Set() && filled.Backoff == 0 {
		filled.Backoff = DefaultRestartBackoff
	}

	return filled, nil
}

// Supervised says the record is one whose guest may be starting processes again right now.
func Supervised(sb models.Sandbox) bool {
	return sb.State == models.StateRunning && len(sb.Processes) > 0
}

// RecordProcesses copies shard-init's table onto every running record, one report line per start again, per policy given up and per process ended.
func (s *Service) RecordProcesses(ctx context.Context, sandboxes []models.Sandbox, report func(string)) error {
	var errs []error
	for _, sb := range sandboxes {
		if !Supervised(sb) {
			continue
		}
		if err := s.recordProcesses(ctx, sb.ID, report); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// recordProcesses reads the record again under the lock, because a stop may have landed since the list.
func (s *Service) recordProcesses(ctx context.Context, id string, report func(string)) error {
	unlock, ok := s.tryLock(id)
	if !ok {
		return nil
	}
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return err
	}

	return s.writeProcesses(ctx, id, sb, report)
}

// writeProcesses copies the table onto a record read under the lock its caller holds.
func (s *Service) writeProcesses(ctx context.Context, id string, sb models.Sandbox, report func(string)) error {
	if !Supervised(sb) {
		return nil
	}

	reports, why, err := s.table(ctx, id)
	if err != nil {
		return err
	}
	if err := s.recordExitChannel(id, sb, why, report); err != nil {
		return err
	}

	next := merged(sb.Processes, reports)
	if slices.EqualFunc(next, sb.Processes, sameStatus) {
		return nil
	}

	err = s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.Processes = merged(rec.Processes, reports)

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s: its processes moved on but its record was not updated: %w", id, err)
	}

	for i, after := range next {
		for _, line := range transitions(id, sb.Processes[i], after) {
			report(line)
		}
	}

	return nil
}

func sameStatus(a, b models.Process) bool {
	x, y := a.Status, b.Status
	sameExit := (x.Exit == nil) == (y.Exit == nil) && (x.Exit == nil || *x.Exit == *y.Exit)

	return x.State == y.State && x.Restarts == y.Restarts && x.StartedAt.Equal(y.StartedAt) && sameExit
}

// transitions are the daemon log lines one process's move earns.
func transitions(id string, before, after models.Process) []string {
	var lines []string
	was, now := before.Status, after.Status

	// A count the reset window zeroed can land on the same number again, so a fresh start time marks a real start too.
	if now.Restarts > 0 && (now.Restarts != was.Restarts || !now.StartedAt.Equal(was.StartedAt)) {
		lines = append(lines, restartReport(id, after.Name, now.Restarts, after.Restart.Retries))
	}
	if now.State == models.ProcessGaveUp && was.State != models.ProcessGaveUp {
		return append(lines, fmt.Sprintf("sandbox %s: process %s exited again and the %d restarts its policy allows are spent", id, after.Name, after.Restart.Retries))
	}
	if now.State.Ended() && !was.State.Ended() {
		lines = append(lines, fmt.Sprintf("sandbox %s: process %s is %s%s", id, after.Name, now.State, exitText(now.Exit)))
	}

	return lines
}

// restartReport names the limit when the policy has one and leaves it off when the restarts are unlimited.
func restartReport(id, name string, count, retries int) string {
	if retries > 0 {
		return fmt.Sprintf("sandbox %s: process %s was started again, %d of %d", id, name, count, retries)
	}

	return fmt.Sprintf("sandbox %s: process %s was started again, %d", id, name, count)
}

func exitText(exit *models.ExitStatus) string {
	if exit == nil {
		return ""
	}

	return fmt.Sprintf(" (code %d, signal %d)", exit.Code, exit.Signal)
}
