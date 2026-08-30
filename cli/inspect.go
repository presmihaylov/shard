package cli

import (
	"context"
	"encoding/json"
	"fmt"
)

// inspect prints the record shard decoded, so a script reads one field with jq.
func (a App) inspect(_ context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("inspect takes one sandbox id, got %d", len(args))
	}

	repo, err := a.deps().repo()
	if err != nil {
		return err
	}

	id, err := repo.Resolve(args[0])
	if err != nil {
		return err
	}

	sb, err := repo.Get(id)
	if err != nil {
		return err
	}

	blob, err := json.MarshalIndent(sb, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the record of sandbox %s: %w", id, err)
	}

	return a.print(string(blob))
}
