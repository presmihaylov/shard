package cli

import (
	"context"
	"fmt"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
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
	c, err := a.client()
	if err != nil {
		return err
	}

	insp, err := c.GetSandbox(ctx, rest[0])
	if err != nil {
		return err
	}
	if format == formatJSON {
		return writeJSON(a.Out, a.inspection(insp))
	}

	sections, err := inspectSections(a.record(insp.Sandbox), insp)
	if err != nil {
		return err
	}

	return writeSections(a.Out, sections...)
}

// record is what a verb prints for a sandbox: the whole record on the daemon host, and through a front the public one it answered, so no host field reads as a zero.
func (a App) record(sb models.Sandbox) any {
	if a.Remote != "" {
		return client.Public(sb)
	}

	return sb
}

func (a App) inspection(insp sandbox.Inspection) any {
	if a.Remote != "" {
		return client.PublicInspection(insp)
	}

	return insp
}
