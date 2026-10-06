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
	id, name, err := s.readyForApp(ctx, ref)
	if err != nil {
		return models.AppExit{}, err
	}

	t, err := s.openLogs(id)
	if err != nil {
		return models.AppExit{}, err
	}
	defer func() { err = errors.Join(err, t.close()) }()

	w, err := open()
	if err != nil {
		return models.AppExit{}, err
	}

	return s.waitApp(ctx, id, name, func() error { return t.follow(w) })
}

// WaitApp answers how the app ended once its restart policy is over, and reads none of its output.
func (s *Service) WaitApp(ctx context.Context, ref string) (models.AppExit, error) {
	id, name, err := s.readyForApp(ctx, ref)
	if err != nil {
		return models.AppExit{}, err
	}

	return s.waitApp(ctx, id, name, func() error { return nil })
}

// waitApp polls the files shard-init writes rather than the record, whose liveness tick is seconds apart.
func (s *Service) waitApp(ctx context.Context, id, name string, copyOut func() error) (models.AppExit, error) {
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
			if err := s.recordAppEnd(ctx, id); err != nil {
				return models.AppExit{}, err
			}

			return appEnd(id, last, count)
		}
		if !status.Alive() {
			return models.AppExit{}, &StateError{Sandbox: name, State: status.State, Fix: "its app had not ended; shard start " + name + " runs it again", Code: models.CodeSandboxNotRunning}
		}

		select {
		case <-ctx.Done():
			return models.AppExit{}, ctx.Err()
		case <-time.After(followInterval):
		}
	}
}

// recordAppEnd makes the tick's two writes under the lock the tick only tries, since the run client reads the record as soon as it has the end.
func (s *Service) recordAppEnd(ctx context.Context, id string) error {
	unlock, err := s.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return err
	}
	// A stop that landed first wrote the exit it saw; past a start or a resume the supervisor holds what this run did, so it is read again.
	if !sb.State.Live() {
		return nil
	}
	if err := s.recordEntrypointExit(ctx, id, sb, s.report); err != nil {
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
	id, name, err := s.readyForApp(ctx, ref)
	if err != nil {
		return err
	}

	count, err := s.cfg.Provider.Restarts(ctx, id)
	if err != nil {
		return fmt.Errorf("read the restart count of sandbox %s: %w", id, err)
	}
	if count.Ended {
		return &StateError{Sandbox: name, State: models.StateRunning, Fix: "its app already ended; shard logs " + name + " shows what it wrote", Code: models.CodeAppEnded}
	}

	return s.cfg.Provider.StopApp(ctx, id, force)
}

// readyForApp is readyForExec for a sandbox that shard run started, since one create made runs no app to attach to or stop.
func (s *Service) readyForApp(ctx context.Context, ref string) (id, name string, err error) {
	id, sb, err := s.resolveForExec(ref)
	if err != nil {
		return "", "", err
	}
	if len(sb.Command) == 0 {
		return "", "", &StateError{Sandbox: nameOf(id, sb), State: sb.State, Fix: "it runs no app; shard run <image> <command> starts a sandbox with one", Code: models.CodeNoApp}
	}

	id, sb, err = s.readyForExec(ctx, id)

	return id, nameOf(id, sb), err
}
