package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/presmihaylov/shard/models"
)

// Autostart brings back what a daemon start owes: each stopped sandbox with a process that start launches, and each such process a running sandbox's guest never got.
func (s *Service) Autostart(ctx context.Context, sandboxes []models.Sandbox, report func(string)) error {
	var errs []error
	for _, sb := range sandboxes {
		if err := s.autostart(ctx, sb.ID, report); err != nil {
			errs = append(errs, fmt.Errorf("sandbox %s: %w", nameOf(sb.ID, sb), err))
		}
	}

	return errors.Join(errs...)
}

// Owed says a daemon start may have something to start in the sandbox: a process whose policy brings it back, in a sandbox that is stopped or running.
func Owed(sb models.Sandbox) bool {
	if sb.State != models.StateStopped && sb.State != models.StateRunning {
		return false
	}

	return slices.ContainsFunc(sb.Processes, func(p models.Process) bool { return launchable(p, sb, daemonStart) })
}

// autostart reads the record again under the lock, because a verb may have moved the sandbox since the list.
func (s *Service) autostart(ctx context.Context, id string, report func(string)) error {
	unlock, err := s.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return err
	}
	if !Owed(sb) {
		return nil
	}

	switch sb.State {
	case models.StateStopped:
		if err := s.start(ctx, id); err != nil {
			return imageGone(nameOf(id, sb), sb.Image, sb.Digest, "start", err)
		}
		report(fmt.Sprintf("sandbox %s: started as the daemon came up, for the processes its restart policies bring back", nameOf(id, sb)))

		return s.launch(ctx, id, daemonStart, nil)
	case models.StateRunning:
		reports, why, err := s.table(ctx, id)
		if err != nil {
			return err
		}
		if why != "" {
			return fmt.Errorf("its processes are not started again while their table is unreadable: %s", why)
		}

		// The guest starts again what it still runs or waits to restart; an ended name goes through the policy like any other.
		live := map[string]bool{}
		for name, r := range latestReports(reports) {
			live[name] = !r.State.Ended()
		}

		return s.launch(ctx, id, daemonStart, live)
	}

	return nil
}

// launch starts each process a start by `by` brings back, except the names in skip; one that fails is recorded and the rest still start.
func (s *Service) launch(ctx context.Context, id string, by trigger, skip map[string]bool) error {
	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return err
	}

	var errs []error
	for _, p := range sb.Processes {
		if skip[p.Name] || !launchable(p, sb, by) {
			continue
		}

		// Each start rewrites the record, so the next one builds on what the last one wrote.
		rec, err := s.cfg.Repo.Get(id)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		if _, err := s.startProcess(ctx, id, rec, slices.Clone(rec.Processes), p); err != nil {
			errs = append(errs, fmt.Errorf("process %s did not start again: %w", p.Name, err))
		}
	}

	return errors.Join(errs...)
}
