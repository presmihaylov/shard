package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/presmihaylov/shard/services/client"
)

// logsOptions is one parsed shard policy logs invocation.
type logsOptions struct {
	id     string
	follow bool
}

// processLogsOptions is one parsed shard logs: the sandbox, the process when one is named, and whether to follow.
type processLogsOptions struct {
	id     string
	name   string
	follow bool
}

// logs is a reader: the daemon streams what one process wrote, and this prints it as it arrives.
func (a App) logs(ctx context.Context, args []string) error {
	opts, err := parseProcessLogs(args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	name := opts.name
	if name == "" {
		if name, err = onlyProcess(ctx, c, opts.id); err != nil {
			return err
		}
	}

	return c.Logs(ctx, opts.id, name, opts.follow, a.Out)
}

// onlyProcess is the process a logs without a name means, which is one only while the sandbox has no other.
func onlyProcess(ctx context.Context, c *client.Client, id string) (string, error) {
	procs, err := c.Processes(ctx, id)
	if err != nil {
		return "", err
	}
	if len(procs) == 0 {
		return "", fmt.Errorf("sandbox %s has no process to show the output of; shard run %s -- COMMAND starts one", id, id)
	}
	if len(procs) > 1 {
		names := make([]string, len(procs))
		for i, p := range procs {
			names[i] = p.Name
		}

		return "", fmt.Errorf("sandbox %s has %d processes, so name one: shard logs %s NAME, with NAME one of %s", id, len(procs), id, strings.Join(names, ", "))
	}

	return procs[0].Name, nil
}

func parseProcessLogs(args []string) (processLogsOptions, error) {
	var opts processLogsOptions

	flags := newFlags("logs")
	flags.BoolVar(&opts.follow, "f", false, "")
	flags.BoolVar(&opts.follow, "follow", false, "")

	refs, rest, err := parseAround(flags, args, 2)
	if err != nil {
		return processLogsOptions{}, err
	}
	if len(refs) == 0 || len(rest) != 0 {
		return processLogsOptions{}, fmt.Errorf("logs takes one sandbox id or name and at most one process name, got %s", gotArgs(append(refs, rest...)))
	}

	opts.id = refs[0]
	if len(refs) == 2 {
		opts.name = refs[1]
	}

	return opts, nil
}

// parseLogs parses policy logs, which takes one sandbox.
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
		return logsOptions{}, fmt.Errorf("%s takes one sandbox id or name, got %s", verb, gotArgs(rest))
	}

	opts.id = rest[0]

	return opts, nil
}
