package cli

import (
	"context"
)

// clone asks the daemon for a new sandbox over a copy of the files another one kept, and prints the new id.
func (a App) clone(ctx context.Context, args []string) error {
	source, req, err := parseCopy("clone", args)
	if err != nil {
		return err
	}

	sb, err := a.client().CloneSandbox(ctx, source, req)
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}
