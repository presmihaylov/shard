package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/presmihaylov/shard/services/sandboxstate"
)

// followInterval is how long a follow waits at the end of the file before it reads again.
const followInterval = 200 * time.Millisecond

// The reasons a follow ends on its own, which the end message of logs?follow=true carries.
const (
	LogsStopped = "stopped"
	LogsRemoved = "removed"
)

// Logs writes what the entrypoint wrote into w. The provider appends it to one file from create on,
// so a stopped sandbox still answers with everything it wrote.
func (s *Service) Logs(_ context.Context, ref string, w io.Writer) (err error) {
	_, f, err := s.openLogs(ref)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	return copyOutput(w, f)
}

// FollowLogs writes the output as it grows and answers why that ended, or nothing when the caller left first.
func (s *Service) FollowLogs(ctx context.Context, ref string, w io.Writer) (reason string, err error) {
	id, f, err := s.openLogs(ref)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	return s.follow(ctx, w, f, id)
}

// openLogs asks the record before the provider, so an id nobody ever created is refused as one.
func (s *Service) openLogs(ref string) (string, *os.File, error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return "", nil, err
	}

	if _, err := s.cfg.Repo.Get(id); err != nil {
		return "", nil, err
	}

	path, err := s.cfg.Provider.LogPath(id)
	if err != nil {
		return "", nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		return "", nil, fmt.Errorf("open the output of sandbox %s: %w", id, err)
	}

	return id, f, nil
}

// follow asks the substrate, not the record, because a record saying running outlives an OOM kill.
func (s *Service) follow(ctx context.Context, w io.Writer, r io.Reader, id string) (string, error) {
	for {
		// The status is read before the copy, so what the entrypoint wrote on its way out is drained.
		status, err := s.cfg.Provider.Status(ctx, id)
		if err != nil {
			return "", err
		}

		if err := copyOutput(w, r); err != nil {
			return "", err
		}

		if !status.Alive() {
			return s.logsEnd(id)
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
	_, err := s.cfg.Repo.Get(id)
	if errors.Is(err, sandboxstate.ErrNotFound) {
		return LogsRemoved, nil
	}
	if err != nil {
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
