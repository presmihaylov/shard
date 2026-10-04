package cli

import (
	"context"
	"fmt"
)

// resume asks the daemon to run a paused sandbox again from its checkpoint.
func (a App) resume(ctx context.Context, args []string) error {
	rest, err := parseArgs("resume", args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("resume takes one sandbox id, got %d", len(rest))
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	sb, err := c.ResumeSandbox(ctx, rest[0])
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}
