package cli

import (
	"context"
	"fmt"
)

// pause asks the daemon to write the sandbox into its checkpoint and prints the id it acted on.
func (a App) pause(ctx context.Context, args []string) error {
	rest, err := parseArgs("pause", args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("pause takes one sandbox id or name, got %s", gotArgs(rest))
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	sb, err := c.PauseSandbox(ctx, rest[0])
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}
