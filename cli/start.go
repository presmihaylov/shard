package cli

import (
	"context"
	"fmt"
)

// start asks the daemon to run a stopped sandbox again and prints the id it acted on.
func (a App) start(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("start takes one sandbox id, got %d", len(args))
	}

	sb, err := a.client().StartSandbox(ctx, args[0])
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}
