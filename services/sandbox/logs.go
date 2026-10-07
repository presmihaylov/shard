package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/presmihaylov/shard/services/sandboxstate"
)

// followInterval is how long a follow waits at the end of the file before it reads again.
const followInterval = 200 * time.Millisecond

// The reasons a follow ends on its own, which the end message of logs?follow=true carries.
const (
	LogsEnded   = "ended"
	LogsStopped = "stopped"
	LogsRemoved = "removed"
)

// Logs writes what one process wrote, every run the host still keeps of it, so a stopped sandbox still answers.
func (s *Service) Logs(_ context.Context, ref, name string, w io.Writer) (err error) {
	id, err := s.logged(ref, name)
	if err != nil {
		return err
	}

	t, err := s.openProcessLog(id, name, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, t.close()) }()

	return t.read(w)
}

// FollowLogs writes one process's output as it grows and answers why that ended, or nothing when the caller left first.
func (s *Service) FollowLogs(ctx context.Context, ref, name string, w io.Writer) (reason string, err error) {
	id, err := s.logged(ref, name)
	if err != nil {
		return "", err
	}

	t, err := s.openProcessLog(id, name, 0)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, t.close()) }()

	return s.follow(ctx, w, t, id, name)
}

// logged asks the record before the provider, so an id nobody ever created and a name it never ran are refused as such.
func (s *Service) logged(ref, name string) (string, error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return "", err
	}

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return "", err
	}

	if err := FailedGuard(id, sb); err != nil {
		return "", err
	}
	if !slices.ContainsFunc(sb.Processes, named(name)) {
		return "", noProcess(id, sb, name)
	}

	return id, nil
}

// follow copies the log until the process or its sandbox ends, or the caller hangs up.
func (s *Service) follow(ctx context.Context, w io.Writer, t *tail, id, name string) (string, error) {
	for {
		// Both are read before the copy, so what the process wrote on its way out is drained.
		seen, err := s.sight(ctx, id, name)
		if err != nil {
			return "", err
		}

		if err := t.follow(w); err != nil {
			return "", err
		}

		if !seen.status.Alive() {
			return s.logsEnd(id)
		}
		if !seen.found || seen.p.Status.State.Ended() {
			return LogsEnded, nil
		}

		select {
		case <-ctx.Done():
			// An operator leaves a follow by hanging up, and that leaves nothing behind on the host.
			return "", nil
		case <-time.After(followInterval):
		}
	}
}

// logsEnd tells a stop from a rm: the substrate forgets both, and only the record outlives a stop.
func (s *Service) logsEnd(id string) (string, error) {
	sb, err := s.cfg.Repo.Get(id)
	if errors.Is(err, sandboxstate.ErrNotFound) {
		return LogsRemoved, nil
	}
	if err != nil {
		return "", err
	}

	// A sandbox that failed under the follow ends it with the refusal a new logs call would answer.
	if err := FailedGuard(id, sb); err != nil {
		return "", err
	}

	return LogsStopped, nil
}

func copyOutput(w io.Writer, r io.Reader) error {
	if _, err := io.Copy(w, r); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}
