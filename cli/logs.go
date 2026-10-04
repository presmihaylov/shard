package cli

import (
	"context"
	"fmt"
)

// logsOptions is one parsed shard logs invocation.
type logsOptions struct {
	id     string
	follow bool
	egress bool
}

// logs is a reader: the daemon streams what the entrypoint wrote, and this prints it as it arrives.
func (a App) logs(ctx context.Context, args []string) error {
	opts, err := parseLogs(args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	if opts.egress && opts.follow {
		return c.FollowEgressLog(ctx, opts.id, a.Out, a.Err)
	}

	if opts.egress {
		return c.EgressLog(ctx, opts.id, a.Out, a.Err)
	}

	return c.Logs(ctx, opts.id, opts.follow, a.Out)
}

func parseLogs(args []string) (logsOptions, error) {
	var opts logsOptions

	flags := newFlags("logs")
	flags.BoolVar(&opts.follow, "f", false, "")
	flags.BoolVar(&opts.follow, "follow", false, "")
	flags.BoolVar(&opts.egress, "egress", false, "")

	if err := parseVerb(flags, args); err != nil {
		return logsOptions{}, err
	}

	rest := flags.Args()
	if len(rest) != 1 {
		return logsOptions{}, fmt.Errorf("logs takes one sandbox id, got %d", len(rest))
	}

	opts.id = rest[0]

	return opts, nil
}
