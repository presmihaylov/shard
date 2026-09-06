package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
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

	if opts.egress {
		return a.client().EgressLog(ctx, opts.id, a.Out)
	}

	return a.client().Logs(ctx, opts.id, opts.follow, a.Out)
}

func parseLogs(args []string) (logsOptions, error) {
	var opts logsOptions

	flags := flag.NewFlagSet("shard logs", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&opts.follow, "f", false, "keep printing until the sandbox stops")
	flags.BoolVar(&opts.egress, "egress", false, "print the egress decisions instead of the entrypoint output")

	if err := flags.Parse(args); err != nil {
		return logsOptions{}, fmt.Errorf("parse the logs flags: %w", err)
	}

	// The egress log is a file that is read once, not a stream, so there is nothing for -f to follow.
	if opts.egress && opts.follow {
		return logsOptions{}, fmt.Errorf("logs --egress cannot follow: drop -f")
	}

	rest := flags.Args()
	if len(rest) != 1 {
		return logsOptions{}, fmt.Errorf("logs takes one sandbox id, got %d", len(rest))
	}

	opts.id = rest[0]

	return opts, nil
}
