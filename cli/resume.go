package cli

import (
	"context"
	"fmt"
)

// resume asks the daemon to run a paused sandbox again from its snapshot.
func (a App) resume(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("resume takes one sandbox id, got %d", len(args))
	}

	sb, err := a.client().ResumeSandbox(ctx, args[0])
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}
