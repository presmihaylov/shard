package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/presmihaylov/shard/models"
)

// checkpointFile is the one file every snapshot holds, and the provider writes it last before it deletes.
const checkpointFile = "checkpoint.img"

// pause writes a running sandbox into its snapshot directory and frees its memory. The record keeps
// the address, the writable layer stays on disk, and resume brings the whole thing back from those.
func (a App) pause(ctx context.Context, args []string) (err error) {
	if len(args) != 1 {
		return fmt.Errorf("pause takes one sandbox id, got %d", len(args))
	}

	d := a.deps()

	repo, err := d.repo()
	if err != nil {
		return err
	}

	provider, err := d.provider()
	if err != nil {
		return err
	}

	if err := requireVerb(provider, models.VerbPause); err != nil {
		return err
	}

	id, err := repo.Resolve(args[0])
	if err != nil {
		return err
	}

	// A stop or a second pause in flight would end or delete the sandbox this one is about to snapshot.
	release, err := repo.Hold(ctx, id)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()

	sb, err := repo.Get(id)
	if err != nil {
		return err
	}

	if sb.State != models.StateRunning {
		return fmt.Errorf("sandbox %s is %s: pause takes a running sandbox", id, sb.State)
	}

	dir, err := repo.SnapshotDir(id)
	if err != nil {
		return err
	}

	if err := provider.Pause(ctx, id, dir); err != nil {
		return errors.Join(err, a.reconcileGone(ctx, repo, provider, id, dir))
	}

	err = repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = models.StatePaused
		sb.PID = 0
		sb.Snapshot = dir

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s is paused but its record was not updated: %w", id, err)
	}

	return a.print(id)
}

// reconcileGone is for a pause that failed: the sandbox still runs and the record is right, or the
// snapshot is complete and only the host cleanup failed, or the substrate lost it on the way.
func (a App) reconcileGone(ctx context.Context, repo sandboxRepo, provider models.Provider, id string, dir string) error {
	status, err := provider.Status(ctx, id)
	if err != nil {
		return err
	}
	if status.Alive() {
		return nil
	}

	// The checkpoint is the last file the provider writes before it deletes, so its presence means paused.
	_, statErr := os.Stat(filepath.Join(dir, checkpointFile))
	state := models.StateStopped
	if statErr == nil {
		state = models.StatePaused
	}

	err = repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = state
		sb.PID = 0
		if state == models.StatePaused {
			sb.Snapshot = dir
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s is gone but its record was not updated: %w", id, err)
	}

	if state == models.StatePaused {
		return fmt.Errorf("sandbox %s is paused, but the host cleanup after the snapshot failed", id)
	}

	return fmt.Errorf("sandbox %s is gone from %s and its record says stopped", id, provider.Name())
}
