package cli

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/presmihaylov/shard/services/sandbox"
)

// snapshotCreate copies the files a stopped sandbox kept into a snapshot, and prints its id.
func (a App) snapshotCreate(ctx context.Context, args []string) error {
	req, err := parseSnapshotCreate(args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	snap, err := c.CreateSnapshot(ctx, req)
	if err != nil {
		return err
	}

	return a.print(snap.ID)
}

// parseSnapshotCreate refuses a name no verb could take back before the daemon copies anything.
func parseSnapshotCreate(args []string) (sandbox.SnapshotRequest, error) {
	var req sandbox.SnapshotRequest

	flags := newFlags("snapshot create")
	flags.StringVar(&req.Name, "name", "", "")

	if err := parseVerb(flags, args); err != nil {
		return sandbox.SnapshotRequest{}, err
	}
	if named(flags) {
		if err := sandbox.ValidSnapshotName(req.Name); err != nil {
			return sandbox.SnapshotRequest{}, fmt.Errorf("snapshot create --name: %w", err)
		}
	}
	if flags.NArg() != 1 {
		return sandbox.SnapshotRequest{}, fmt.Errorf("snapshot create takes one sandbox id or name, got %s", gotArgs(flags.Args()))
	}

	req.Sandbox = flags.Arg(0)

	return req, nil
}

func (a App) snapshotList(ctx context.Context, args []string) error {
	rest, format, err := parseFormatArgs("snapshot list", args, formatTable)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("snapshot list takes no arguments, got %s", gotArgs(rest))
	}
	c, err := a.client()
	if err != nil {
		return err
	}

	snaps, err := c.ListSnapshots(ctx)
	if err != nil {
		return err
	}
	if format == formatJSON {
		return writeJSON(a.Out, nonNil(snaps))
	}

	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSOURCE\tIMAGE\tSIZE\tCREATED")

	for _, snap := range snaps {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", snap.ID, snap.Name, snap.Source, snap.Image, humanSize(snap.Size), humanAge(snap.CreatedAt))
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

func (a App) snapshotInspect(ctx context.Context, args []string) error {
	rest, format, err := parseFormatArgs("snapshot inspect", args, formatJSON)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("snapshot inspect takes one snapshot id or name, got %s", gotArgs(rest))
	}
	c, err := a.client()
	if err != nil {
		return err
	}

	snap, err := c.InspectSnapshot(ctx, rest[0])
	if err != nil {
		return err
	}

	if format == formatJSON {
		return writeJSON(a.Out, snap)
	}

	fields, err := fieldSection(snap)
	if err != nil {
		return err
	}

	return writeSections(a.Out, fields)
}

func (a App) snapshotRemove(ctx context.Context, args []string) error {
	rest, err := parseArgs("snapshot remove", args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("snapshot remove takes one snapshot id or name, got %s", gotArgs(rest))
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	if err := c.RemoveSnapshot(ctx, rest[0]); err != nil {
		return err
	}

	return a.print(rest[0])
}
