package cli

import (
	"context"
	"fmt"
)

// kill ends one process and cancels its restarts, and prints its name once it is reaped; the sandbox stays running.
func (a App) kill(ctx context.Context, args []string) error {
	flags := newFlags("kill")
	var force bool
	flags.BoolVar(&force, "force", false, "")

	refs, rest, err := parseAround(flags, args, 2)
	if err != nil {
		return err
	}
	if len(refs) != 2 || len(rest) != 0 {
		return fmt.Errorf("kill takes one sandbox id or name and one process name, got %s", gotArgs(append(refs, rest...)))
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	p, err := c.Kill(ctx, refs[0], refs[1], force)
	if err != nil {
		return err
	}

	return a.print(p.Name)
}
