package cli

import (
	"context"
	"encoding/json"
	"fmt"
)

// inspect prints the record the daemon decoded, so a script reads one field with jq.
func (a App) inspect(ctx context.Context, args []string) error {
	rest, format, err := parseFormatArgs("inspect", args, formatJSON)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("inspect takes one sandbox id, got %d", len(rest))
	}
	if err := formatLanded("inspect", format, formatJSON); err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	sb, err := c.GetSandbox(ctx, rest[0])
	if err != nil {
		return err
	}

	blob, err := json.MarshalIndent(sb, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the record of sandbox %s: %w", sb.ID, err)
	}

	return a.print(string(blob))
}
