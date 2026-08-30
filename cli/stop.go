package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/presmihaylov/shard/models"
)

// DefaultStopGrace is how long the entrypoint gets to answer SIGTERM before shard kills it.
const DefaultStopGrace = 10 * time.Second

// stopOptions is one parsed shard stop invocation.
type stopOptions struct {
	id    string
	grace time.Duration
}

func (a App) stop(ctx context.Context, args []string) (err error) {
	opts, err := parseStop(args)
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

	release, err := repo.Hold(ctx, opts.id)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()

	if err := a.stopSandbox(ctx, repo, provider, opts.id, opts.grace); err != nil {
		return err
	}

	return a.print(opts.id)
}

// stopSandbox ends the processes and keeps everything rm frees: the record, the lease, the address
// and the writable layer all outlive it, so SHARD-96 can start the sandbox again.
func (a App) stopSandbox(ctx context.Context, repo sandboxRepo, provider models.Provider, id string, grace time.Duration) error {
	sb, err := repo.Get(id)
	if err != nil {
		return err
	}

	// A second stop changes nothing: the exit status the first one recorded is the one that happened.
	// Unless a start failed after the substrate came up, which is the one way a stopped record lies.
	if sb.State == models.StateStopped {
		status, err := provider.Status(ctx, id)
		if err != nil {
			return err
		}
		if !status.Alive() {
			return nil
		}
	}

	if err := provider.Stop(ctx, id, grace); err != nil {
		return err
	}

	exit, err := lastExit(ctx, provider, id)
	if err != nil {
		return err
	}

	return repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = models.StateStopped
		sb.PID = 0
		if exit != nil {
			sb.ExitStatus = exit
		}

		return nil
	})
}

// lastExit reads how the entrypoint ended, once the sandbox is already stopped. A sandbox the grace
// ran out on was killed, so its supervisor never recorded one, and that is an outcome not a failure.
func lastExit(ctx context.Context, provider models.Provider, id string) (*models.ExitStatus, error) {
	status, err := provider.Wait(ctx, id)
	if errors.Is(err, models.ErrNoExitStatus) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &status, nil
}

func parseStop(args []string) (stopOptions, error) {
	var opts stopOptions

	flags := flag.NewFlagSet("shard stop", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.DurationVar(&opts.grace, "time", DefaultStopGrace, "how long the entrypoint gets before it is killed")

	if err := flags.Parse(args); err != nil {
		return stopOptions{}, fmt.Errorf("parse the stop flags: %w", err)
	}

	// A grace below zero is not a spelling of kill it now, which is what zero already spells.
	if opts.grace < 0 {
		return stopOptions{}, fmt.Errorf("--time is how long the entrypoint gets and cannot be negative, got %s", opts.grace)
	}

	rest := flags.Args()
	if len(rest) != 1 {
		return stopOptions{}, fmt.Errorf("stop takes one sandbox id, got %d", len(rest))
	}

	opts.id = rest[0]

	return opts, nil
}
