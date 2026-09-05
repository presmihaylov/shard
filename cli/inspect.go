package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/sandbox"
)

// inspected is the record plus what the host enforces for it, which the record only names.
type inspected struct {
	models.Sandbox
	Egress *egress.Effective `json:"egress,omitempty"`
}

// inspect prints the record shard decoded, so a script reads one field with jq.
func (a App) inspect(_ context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("inspect takes one sandbox id, got %d", len(args))
	}

	d := a.deps()

	repo, err := d.repo()
	if err != nil {
		return err
	}

	sb, err := sandbox.Get(repo, args[0])
	if err != nil {
		return err
	}

	out := inspected{Sandbox: sb}
	if sb.Policy != "" {
		source, err := d.egress()
		if err != nil {
			return err
		}

		effective, err := source.Effective(sb)
		if err != nil {
			return err
		}
		out.Egress = &effective
	}

	blob, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the record of sandbox %s: %w", sb.ID, err)
	}

	return a.print(string(blob))
}
