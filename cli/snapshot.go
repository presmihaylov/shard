package cli

import (
	"context"
	"fmt"

	"github.com/presmihaylov/shard/services/sandbox"
)

// The snapshot verbs parse as they will once SHARD-457a lands, then refuse without a call to the daemon.

func (a App) snapshotCreate(_ context.Context, args []string) error {
	if _, err := parseSnapshotCreate(args); err != nil {
		return err
	}

	return notImplemented("snapshot create")
}

func parseSnapshotCreate(args []string) (sandbox.SnapshotRequest, error) {
	var req sandbox.SnapshotRequest

	flags := newFlags("snapshot create")
	flags.StringVar(&req.Name, "name", "", "")

	if err := parseVerb(flags, args); err != nil {
		return sandbox.SnapshotRequest{}, err
	}
	// A snapshot name is a link name under the root, as a sandbox name is, so it takes the same rules.
	if named(flags) {
		if err := sandbox.ValidName(req.Name); err != nil {
			return sandbox.SnapshotRequest{}, fmt.Errorf("snapshot create --name: %w", err)
		}
	}
	if flags.NArg() != 1 {
		return sandbox.SnapshotRequest{}, fmt.Errorf("snapshot create takes one sandbox id, got %d", flags.NArg())
	}

	req.Sandbox = flags.Arg(0)

	return req, nil
}

func (a App) snapshotList(_ context.Context, args []string) error {
	rest, _, err := parseFormatArgs("snapshot list", args, formatTable)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("snapshot list takes no arguments, got %d", len(rest))
	}

	return notImplemented("snapshot list")
}

func (a App) snapshotInspect(_ context.Context, args []string) error {
	rest, _, err := parseFormatArgs("snapshot inspect", args, formatJSON)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("snapshot inspect takes one snapshot id, got %d", len(rest))
	}

	return notImplemented("snapshot inspect")
}

func (a App) snapshotRemove(_ context.Context, args []string) error {
	rest, err := parseArgs("snapshot remove", args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("snapshot remove takes one snapshot id, got %d", len(rest))
	}

	return notImplemented("snapshot remove")
}
