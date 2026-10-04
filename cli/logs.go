package cli

import (
	"context"
	"fmt"
)

// logsOptions is one parsed shard logs or shard policy logs invocation.
type logsOptions struct {
	id     string
	follow bool
}

// logs is a reader: the daemon streams what the entrypoint wrote, and this prints it as it arrives.
func (a App) logs(ctx context.Context, args []string) error {
	opts, err := parseLogs("logs", args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	return c.Logs(ctx, opts.id, opts.follow, a.Out)
}

// parseLogs parses logs and policy logs, which take the same flags and one sandbox.
func parseLogs(verb string, args []string) (logsOptions, error) {
	var opts logsOptions

	flags := newFlags(verb)
	flags.BoolVar(&opts.follow, "f", false, "")
	flags.BoolVar(&opts.follow, "follow", false, "")

	if err := parseVerb(flags, args); err != nil {
		return logsOptions{}, err
	}

	rest := flags.Args()
	if len(rest) != 1 {
		return logsOptions{}, fmt.Errorf("%s takes one sandbox id, got %d", verb, len(rest))
	}

	opts.id = rest[0]

	return opts, nil
}
