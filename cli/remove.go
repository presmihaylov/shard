package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/presmihaylov/shard/services/client"
)

// removeOptions is one parsed shard remove invocation.
type removeOptions struct {
	id    string
	force bool
}

// remove reads the record before the delete, because a delete answers no record and the id printed is the resolved one.
func (a App) remove(ctx context.Context, args []string) error {
	opts, err := parseRemove(args)
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

	// A remove that waited on another remove finds the same nothing.
	err = c.RemoveSandbox(ctx, sb.ID, opts.force)
	if errors.As(err, &missing) {
		return a.removeMissing(opts, err)
	}
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}

// removeMissing fails a plain remove of an id with no record, and lets --force pass it with a warning, as rm -f does.
func (a App) removeMissing(opts removeOptions, err error) error {
	if !opts.force {
		return err
	}

	// The warning names what the operator typed, which is a name that resolved to nothing.
	a.warn(fmt.Sprintf("sandbox %s does not exist, so there is nothing to remove", opts.id))

	return nil
}

func parseRemove(args []string) (removeOptions, error) {
	var opts removeOptions

	flags := newFlags("remove")
	flags.BoolVar(&opts.force, "force", false, "")

	if err := parseVerb(flags, args); err != nil {
		return removeOptions{}, err
	}

	rest := flags.Args()
	if len(rest) != 1 {
		return removeOptions{}, fmt.Errorf("remove takes one sandbox id or name, got %s", gotArgs(rest))
	}

	opts.id = rest[0]

	return opts, nil
}
