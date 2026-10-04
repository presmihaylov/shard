package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/presmihaylov/shard/models"
)

// AttachApp copies the app's log into open's writer until the policy ends; open runs after every refusal, so a refusal precedes the answer.
func (s *Service) AttachApp(ctx context.Context, ref string, open func() (io.Writer, error)) (exit models.AppExit, err error) {
	id, run, err := s.readyForApp(ctx, ref)
	if err != nil {
		return models.AppExit{}, err
	}

	_, t, err := s.openLogs(id)
	if err != nil {
		return models.AppExit{}, err
	}
	defer func() { err = errors.Join(err, t.close()) }()

	w, err := open()
	if err != nil {
		return models.AppExit{}, err
	}

	return s.waitApp(ctx, id, run, func() error { return t.follow(w) })
}

// WaitApp answers how the app ended once its restart policy is over, and reads none of its output.
func (s *Service) WaitApp(ctx context.Context, ref string) (models.AppExit, error) {
	id, run, err := s.readyForApp(ctx, ref)
	if err != nil {
		return models.AppExit{}, err
	}

	return s.waitApp(ctx, id, run, func() error { return nil })
}

// waitApp polls the files shard-init writes rather than the record, whose liveness tick is seconds apart.
func (s *Service) waitApp(ctx context.Context, id string, run time.Time, copyOut func() error) (models.AppExit, error) {
	for {
		// Everything is read before the copy, so what the app wrote on its way out is drained.
		status, err := s.cfg.Provider.Status(ctx, id)
		if err != nil {
			return models.AppExit{}, err
		}
		count, err := s.cfg.Provider.Restarts(ctx, id)
		if err != nil {
			return models.AppExit{}, fmt.Errorf("read the restart count of sandbox %s: %w", id, err)
		}
		last, err := s.cfg.Provider.ExitStatus(ctx, id)
		if err != nil {
			return models.AppExit{}, fmt.Errorf("read the app exit of sandbox %s: %w", id, err)
		}

		if err := copyOut(); err != nil {
			return models.AppExit{}, err
		}

		if count.Ended {
			// The tick copies the exit and the count seconds later, so an inspect right after the run would read neither (SHARD-479).
			if err := s.recordAppEnd(ctx, id, run, last); err != nil {
				return models.AppExit{}, err
			}

			return appEnd(id, last, count)
		}
		if !status.Alive() {
			return models.AppExit{}, &StateError{ID: id, State: status.State, Fix: "its app had not ended; shard start " + id + " runs it again", Code: models.CodeSandboxNotRunning}
		}

		select {
		case <-ctx.Done():
			return models.AppExit{}, ctx.Err()
		case <-time.After(followInterval):
		}
	}
}

// recordAppEnd waits for the lock the tick only tries, since the run client reads the record as soon as it has the end.
func (s *Service) recordAppEnd(ctx context.Context, id string, run time.Time, last *models.ExitStatus) error {
	unlock, err := s.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return err
	}
	// A stop that landed first wrote the exit it saw, and a start since then began a run this exit is not from.
	if !sb.State.Live() || !sb.StartedAt.Equal(run) {
		return nil
	}
	if err := s.recordExit(id, sb, last, s.report); err != nil {
		return err
	}

	return s.writeRestarts(ctx, id, sb, s.report)
}

// appEnd needs no copy of its own: shard-init reports the end only once the host log holds the app's last byte.
func appEnd(id string, last *models.ExitStatus, count models.RestartCount) (models.AppExit, error) {
	if last == nil {
		return models.AppExit{}, fmt.Errorf("sandbox %s: the app ended and left no exit status", id)
	}

	return models.AppExit{Code: last.Code, Signal: last.Signal, Restarts: count.Count}, nil
}

// StopApp cancels every start again of the app and terms it, or kills it with force; the sandbox stays running.
func (s *Service) StopApp(ctx context.Context, ref string, force bool) error {
	id, _, err := s.readyForApp(ctx, ref)
	if err != nil {
		return err
	}

	count, err := s.cfg.Provider.Restarts(ctx, id)
	if err != nil {
		return fmt.Errorf("read the restart count of sandbox %s: %w", id, err)
	}
	if count.Ended {
		return &StateError{ID: id, State: models.StateRunning, Fix: "its app already ended; shard logs " + id + " shows what it wrote", Code: models.CodeAppEnded}
	}

	return s.cfg.Provider.StopApp(ctx, id, force)
}

// readyForApp is readyForExec for a sandbox shard run started, plus when its run began, so the app's end never lands on a later run.
func (s *Service) readyForApp(ctx context.Context, ref string) (string, time.Time, error) {
	id, sb, err := s.resolveForExec(ref)
	if err != nil {
		return "", time.Time{}, err
	}
	if len(sb.Command) == 0 {
		return "", time.Time{}, &StateError{ID: id, State: sb.State, Fix: "it runs no app; shard run <image> <command> starts a sandbox with one", Code: models.CodeNoApp}
	}

	id, err = s.readyForExec(ctx, id)
	if err != nil {
		return "", time.Time{}, err
	}

	return id, sb.StartedAt, nil
}
