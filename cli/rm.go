package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

// rmOptions is one parsed shard rm invocation.
type rmOptions struct {
	id    string
	force bool
	grace time.Duration
}

// remove reads the record before the delete, because a delete answers no record and the id printed is the resolved one.
func (a App) remove(ctx context.Context, args []string) error {
	opts, err := parseRm(args)
	if err != nil {
		return err
	}

	c := a.client()

	sb, err := c.GetSandbox(ctx, opts.id)

	var missing *client.NotFoundError
	if errors.As(err, &missing) {
		// The warning names what the operator typed, which is a name that resolved to nothing.
		a.warn(fmt.Sprintf("sandbox %s does not exist, so there is nothing to remove", opts.id))

		return nil
	}
	if err != nil {
		return err
	}

	// An rm that waited on another rm finds the same nothing, and that is still nothing to report.
	err = c.RemoveSandbox(ctx, sb.ID, opts.force, opts.grace)
	if errors.As(err, &missing) {
		a.warn(fmt.Sprintf("sandbox %s does not exist, so there is nothing to remove", opts.id))

		return nil
	}
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}

func parseRm(args []string) (rmOptions, error) {
	var opts rmOptions

	flags := flag.NewFlagSet("shard rm", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&opts.force, "force", false, "stop the sandbox first if it is still up")
	flags.DurationVar(&opts.grace, "time", sandbox.DefaultStopGrace, "how long --force gives the entrypoint before it is killed")

	if err := flags.Parse(args); err != nil {
		return rmOptions{}, fmt.Errorf("parse the rm flags: %w", err)
	}

	if opts.grace < 0 {
		return rmOptions{}, fmt.Errorf("--time is how long the entrypoint gets and cannot be negative, got %s", opts.grace)
	}

	rest := flags.Args()
	if len(rest) != 1 {
		return rmOptions{}, fmt.Errorf("rm takes one sandbox id, got %d", len(rest))
	}

	opts.id = rest[0]

	return opts, nil
}
