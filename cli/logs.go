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
}

// logs is a reader: the daemon streams what the entrypoint wrote, and this prints it as it arrives.
func (a App) logs(ctx context.Context, args []string) error {
	opts, err := parseLogs(args)
	if err != nil {
		return err
	}

	return a.client().Logs(ctx, opts.id, opts.follow, a.Out)
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
