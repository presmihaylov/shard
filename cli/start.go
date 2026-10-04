package cli

import (
	"context"
	"fmt"
)

// start asks the daemon to run a stopped sandbox again and prints the id it acted on.
func (a App) start(ctx context.Context, args []string) error {
	rest, err := parseArgs("start", args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("start takes one sandbox id or name, got %s", gotArgs(rest))
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	sb, err := c.StartSandbox(ctx, rest[0])
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}
