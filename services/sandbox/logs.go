package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// followInterval is how long a follow waits at the end of the file before it reads again.
const followInterval = 200 * time.Millisecond

// Logs writes what the entrypoint wrote into w. The provider appends it to one file from create on,
// so a stopped sandbox still answers with everything it wrote.
func (s *Service) Logs(ctx context.Context, ref string, follow bool, w io.Writer) (err error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return err
	}

	// The record answers for an id nobody ever created before the provider is asked for a path.
	if _, err := s.cfg.Repo.Get(id); err != nil {
		return err
	}

	path, err := s.cfg.Provider.LogPath(id)
	if err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open the output of sandbox %s: %w", id, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	if !follow {
		return copyOutput(w, f)
	}

	return s.follow(ctx, w, f, id)
}

// follow asks the substrate, not the record, because a record saying running outlives an OOM kill.
func (s *Service) follow(ctx context.Context, w io.Writer, r io.Reader, id string) error {
	for {
		// The status is read before the copy, so what the entrypoint wrote on its way out is drained.
		status, err := s.cfg.Provider.Status(ctx, id)
		if err != nil {
			return err
		}

		if err := copyOutput(w, r); err != nil {
			return err
		}

		if !status.Alive() {
			return nil
		}

		select {
		case <-ctx.Done():
			// An operator leaves a follow by hanging up, and that leaves nothing behind on the host.
			return nil
		case <-time.After(followInterval):
		}
	}
}

func copyOutput(w io.Writer, r io.Reader) error {
	if _, err := io.Copy(w, r); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}
