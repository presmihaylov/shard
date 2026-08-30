package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/presmihaylov/shard/models"
)

// followInterval is how long a follow waits at the end of the file before it reads again.
const followInterval = 200 * time.Millisecond

// logsOptions is one parsed shard logs invocation.
type logsOptions struct {
	id     string
	follow bool
}

// logs is a reader: the provider appends the output to one file from create on, and this prints it.
func (a App) logs(ctx context.Context, args []string) (err error) {
	opts, err := parseLogs(args)
	if err != nil {
		return err
	}

	d := a.deps()

	repo, err := d.repo()
	if err != nil {
		return err
	}

	provider, err := d.provider()
	if err != nil {
		return err
	}

	opts.id, err = repo.Resolve(opts.id)
	if err != nil {
		return err
	}

	// The record answers for an id nobody ever created before the provider is asked for a path.
	if _, err := repo.Get(opts.id); err != nil {
		return err
	}

	path, err := provider.LogPath(opts.id)
	if err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open the output of sandbox %s: %w", opts.id, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	if !opts.follow {
		return copyOutput(a.Out, f)
	}

	return follow(ctx, a.Out, f, provider, opts.id)
}

// follow asks the substrate, not the record, because a record saying running outlives an OOM kill.
func follow(ctx context.Context, w io.Writer, r io.Reader, provider models.Provider, id string) error {
	for {
		// The status is read before the copy, so what the entrypoint wrote on its way out is drained.
		status, err := provider.Status(ctx, id)
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
			// An interrupt is how an operator leaves a follow, and it leaves nothing behind on the host.
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

func parseLogs(args []string) (logsOptions, error) {
	var opts logsOptions

	flags := flag.NewFlagSet("shard logs", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&opts.follow, "f", false, "keep printing until the sandbox stops")

	if err := flags.Parse(args); err != nil {
		return logsOptions{}, fmt.Errorf("parse the logs flags: %w", err)
	}

	rest := flags.Args()
	if len(rest) != 1 {
		return logsOptions{}, fmt.Errorf("logs takes one sandbox id, got %d", len(rest))
	}

	opts.id = rest[0]

	return opts, nil
}
