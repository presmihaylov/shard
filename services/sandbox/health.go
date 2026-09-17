package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
)

// The probe settings a create leaves at zero.
const (
	DefaultHealthInterval = 30
	DefaultHealthTimeout  = 10
	DefaultHealthRetries  = 3
)

// validHealthCheck refuses a probe that names no command, or a setting below what the probe needs.
func validHealthCheck(hc models.HealthCheck) error {
	if len(hc.Command) == 0 {
		return errors.New("health names no command")
	}
	if hc.Interval < 0 || hc.Timeout < 0 || hc.Retries < 0 {
		return errors.New("health.interval, timeout and retries are counts and cannot be negative")
	}

	return nil
}

// withHealthDefaults fills what the request left at zero, so the record reads the settings the daemon uses.
func withHealthDefaults(hc *models.HealthCheck) *models.HealthCheck {
	if hc == nil {
		return nil
	}

	filled := *hc
	if filled.Interval == 0 {
		filled.Interval = DefaultHealthInterval
	}
	if filled.Timeout == 0 {
		filled.Timeout = DefaultHealthTimeout
	}
	if filled.Retries == 0 {
		filled.Retries = DefaultHealthRetries
	}

	return &filled
}

// startingHealth is what a run that has a probe reads before the first one, and nil for a run that has none.
func startingHealth(hc *models.HealthCheck) *models.Health {
	if hc == nil {
		return nil
	}

	return &models.Health{Status: models.HealthStarting}
}

// CheckHealth probes every running record whose probe is due, side by side, and reports each change of status.
func (s *Service) CheckHealth(ctx context.Context, sandboxes []models.Sandbox, now time.Time, report func(string)) error {
	var wg sync.WaitGroup
	errs := make([]error, len(sandboxes))
	for i, sb := range sandboxes {
		if sb.State != models.StateRunning || !probeDue(sb, now) {
			continue
		}
		wg.Go(func() { errs[i] = s.probeAndRecord(ctx, sb, now, report) })
	}
	wg.Wait()

	return errors.Join(errs...)
}

// probeDue says the run has a probe and its interval has passed since the last one, or none ran yet.
func probeDue(sb models.Sandbox, now time.Time) bool {
	if sb.HealthCheck == nil || sb.Health == nil {
		return false
	}

	return !now.Before(sb.Health.CheckedAt.Add(time.Duration(sb.HealthCheck.Interval) * time.Second))
}

// probeAndRecord runs the probe without the lock, because it takes up to the timeout and a stop must not wait on it.
func (s *Service) probeAndRecord(ctx context.Context, sb models.Sandbox, now time.Time, report func(string)) error {
	failure := s.probe(ctx, sb)
	// A daemon on its way down cut the probe short, which says nothing about the sandbox.
	if ctx.Err() != nil {
		return nil
	}

	unlock := s.lock(sb.ID)
	defer unlock()

	// A stop, or a stop and a start, landed while the probe ran: the result is about a run that is over.
	current, err := s.cfg.Repo.Get(sb.ID)
	if err != nil {
		return err
	}
	if current.State != models.StateRunning || current.PID != sb.PID {
		return nil
	}

	var was, is models.HealthStatus
	err = s.cfg.Repo.Update(sb.ID, func(rec *models.Sandbox) error {
		was = rec.Health.Status
		rec.Health.CheckedAt = now
		if failure == nil {
			rec.Health.Status = models.HealthHealthy
			rec.Health.Failures = 0
		}
		if failure != nil {
			rec.Health.Failures++
			if rec.Health.Failures >= rec.HealthCheck.Retries {
				rec.Health.Status = models.HealthUnhealthy
			}
		}
		is = rec.Health.Status

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s was probed but its record was not updated: %w", sb.ID, err)
	}

	if is == was {
		return nil
	}
	if failure != nil {
		report(fmt.Sprintf("sandbox %s is %s: %v", sb.ID, is, failure))

		return nil
	}
	report(fmt.Sprintf("sandbox %s is %s", sb.ID, is))

	return nil
}

// probe answers nil when the probe passed, and otherwise why it did not, in the words the log carries.
func (s *Service) probe(ctx context.Context, sb models.Sandbox) error {
	timeout := time.Duration(sb.HealthCheck.Timeout) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := s.probeCommand(ctx, sb.ID, sb.HealthCheck.Command)
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("the probe did not answer within %s", timeout)
	}

	return err
}

// probeCommand passes on exit 0. A signal or a provider that could not run it are the other exits, so both fail.
func (s *Service) probeCommand(ctx context.Context, id string, argv []string) error {
	exit, err := s.cfg.Provider.Exec(ctx, id, models.ExecSpec{Argv: argv})
	if err != nil {
		return fmt.Errorf("run %s: %w", strings.Join(argv, " "), err)
	}
	if exit.Signal != 0 {
		return fmt.Errorf("%s ended by signal %d", strings.Join(argv, " "), exit.Signal)
	}
	if exit.Code != 0 {
		return fmt.Errorf("%s exited %d", strings.Join(argv, " "), exit.Code)
	}

	return nil
}
