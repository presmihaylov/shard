package cli

import (
	"context"
	"encoding/json"
	"fmt"
)

// inspect prints the record the daemon decoded, so a script reads one field with jq.
func (a App) inspect(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("inspect takes one sandbox id, got %d", len(args))
	}

	sb, err := a.client().GetSandbox(ctx, args[0])
	if err != nil {
		return err
	}

	blob, err := json.MarshalIndent(sb, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the record of sandbox %s: %w", sb.ID, err)
	}

	return a.print(string(blob))
}
