package cli

import (
	"context"
	"fmt"
)

// stopOptions is one parsed shard stop invocation.
type stopOptions struct {
	id string
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

	sb, err := c.StopSandbox(ctx, opts.id)
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}

func parseStop(args []string) (stopOptions, error) {
	var opts stopOptions

	flags := newFlags("stop")

	if err := parseVerb(flags, args); err != nil {
		return stopOptions{}, err
	}

	rest := flags.Args()
	if len(rest) != 1 {
		return stopOptions{}, fmt.Errorf("stop takes one sandbox id, got %d", len(rest))
	}

	opts.id = rest[0]

	return opts, nil
}
