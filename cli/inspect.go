package cli

import (
	"context"
	"fmt"
)

// inspect prints the record the daemon decoded, so a script reads one field with jq.
func (a App) inspect(ctx context.Context, args []string) error {
	rest, format, err := parseFormatArgs("inspect", args, formatJSON)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("inspect takes one sandbox id or name, got %s", gotArgs(rest))
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
		return writeJSON(a.Out, insp)
	}

	sections, err := inspectSections(insp)
	if err != nil {
		return err
	}

	return writeSections(a.Out, sections...)
}
