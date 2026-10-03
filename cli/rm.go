package cli

import (
	"context"
	"errors"
	"fmt"
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

	c, err := a.client()
	if err != nil {
		return err
	}

	sb, err := c.GetSandbox(ctx, opts.id)

	var missing *client.NotFoundError
	if errors.As(err, &missing) {
		return a.removeMissing(opts, err)
	}
	if err != nil {
		return err
	}

	// An rm that waited on another rm finds the same nothing.
	err = c.RemoveSandbox(ctx, sb.ID, opts.force, opts.grace)
	if errors.As(err, &missing) {
		return a.removeMissing(opts, err)
	}
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}

// removeMissing fails a plain rm of an id with no record, and lets --force pass it with a warning, as rm -f does.
func (a App) removeMissing(opts rmOptions, err error) error {
	if !opts.force {
		return err
	}

	// The warning names what the operator typed, which is a name that resolved to nothing.
	a.warn(fmt.Sprintf("sandbox %s does not exist, so there is nothing to remove", opts.id))

	return nil
}

func parseRm(args []string) (rmOptions, error) {
	var opts rmOptions

	flags := newFlags("rm")
	flags.BoolVar(&opts.force, "force", false, "")
	flags.DurationVar(&opts.grace, "time", sandbox.DefaultStopGrace, "")

	if err := parseVerb(flags, args); err != nil {
		return rmOptions{}, err
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
