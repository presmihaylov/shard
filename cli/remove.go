package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/presmihaylov/shard/services/client"
)

// removeOptions is one parsed shard remove invocation.
type removeOptions struct {
	ids   []string
	force bool
}

func (a App) remove(ctx context.Context, args []string) error {
	opts, err := parseRemove(args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	return a.each(ctx, opts.ids, func(ref string) error { return a.removeOne(ctx, c, ref, opts.force) })
}

// removeOne reads the record before the delete, because a delete answers no record and the id printed is the resolved one.
func (a App) removeOne(ctx context.Context, c *client.Client, ref string, force bool) error {
	sb, err := c.GetSandbox(ctx, ref)

	var missing *client.NotFoundError
	if errors.As(err, &missing) {
		return a.removeMissing(ref, force, err)
	}
	if err != nil {
		return err
	}

	// A remove that waited on another remove finds the same nothing.
	err = c.RemoveSandbox(ctx, sb.ID, force)
	if errors.As(err, &missing) {
		return a.removeMissing(ref, force, err)
	}
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}

// removeMissing fails a plain remove of an id with no record, and lets --force pass it with a warning, as rm -f does.
func (a App) removeMissing(ref string, force bool, err error) error {
	if !force {
		return err
	}

	// The warning names what the operator typed, which is a name that resolved to nothing.
	a.warn(fmt.Sprintf("sandbox %s does not exist, so there is nothing to remove", ref))

	return nil
}

func parseRemove(args []string) (removeOptions, error) {
	var opts removeOptions

	flags := newFlags("remove")
	flags.BoolVar(&opts.force, "force", false, "")

	if err := parseVerb(flags, args); err != nil {
		return removeOptions{}, err
	}

	ids, err := sandboxRefs("remove", flags.Args())
	if err != nil {
		return removeOptions{}, err
	}
	opts.ids = ids

	return opts, nil
}
