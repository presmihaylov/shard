package cli

import (
	"context"
	"fmt"
)

// pause asks the daemon to write the sandbox into its snapshot and prints the id it acted on.
func (a App) pause(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("pause takes one sandbox id, got %d", len(args))
	}

	sb, err := a.client().PauseSandbox(ctx, args[0])
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}
