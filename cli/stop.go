package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/presmihaylov/shard/services/sandbox"
)

// stopOptions is one parsed shard stop invocation.
type stopOptions struct {
	id    string
	grace time.Duration
}

// stop asks the daemon to end the sandbox and prints the id it acted on, which is never the name typed.
func (a App) stop(ctx context.Context, args []string) error {
	opts, err := parseStop(args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	sb, err := c.StopSandbox(ctx, opts.id, opts.grace)
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}

func parseStop(args []string) (stopOptions, error) {
	var opts stopOptions

	flags := newFlags("stop")
	flags.DurationVar(&opts.grace, "time", sandbox.DefaultStopGrace, "")

	if err := parseVerb(flags, args); err != nil {
		return stopOptions{}, err
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
