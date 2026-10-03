package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/presmihaylov/shard/services/client"
)

// rmOptions is one parsed shard rm invocation.
type rmOptions struct {
	id    string
	force bool
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
	err = c.RemoveSandbox(ctx, sb.ID, opts.force)
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

	if err := parseVerb(flags, args); err != nil {
		return rmOptions{}, err
	}

	rest := flags.Args()
	if len(rest) != 1 {
		return rmOptions{}, fmt.Errorf("rm takes one sandbox id, got %d", len(rest))
	}

	opts.id = rest[0]

	return opts, nil
}
