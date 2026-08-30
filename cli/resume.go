package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/presmihaylov/shard/models"
)

// resume runs a paused sandbox again from its snapshot. It is the run the pause froze, so the record
// keeps the exit its entrypoint may already have had, and the snapshot stays for the next resume.
func (a App) resume(ctx context.Context, args []string) (err error) {
	if len(args) != 1 {
		return fmt.Errorf("resume takes one sandbox id, got %d", len(args))
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

	id, err := repo.Resolve(args[0])
	if err != nil {
		return err
	}

	// Two resumes of one sandbox would each restore it; the second waits and then sees it running.
	release, err := repo.Hold(ctx, id)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()

	sb, err := repo.Get(id)
	if err != nil {
		return err
	}

	if sb.State != models.StatePaused {
		return fmt.Errorf("sandbox %s is %s: resume takes a paused sandbox", id, sb.State)
	}
	if sb.Snapshot == "" {
		return fmt.Errorf("sandbox %s is paused but records no snapshot to resume from", id)
	}

	net, err := d.net()
	if err != nil {
		return err
	}

	// The lease survived the pause, so this hands back the same address over a namespace built again.
	if _, err := net.Allocate(ctx, id); err != nil {
		return err
	}

	if err := provider.Resume(ctx, id, sb.Snapshot); err != nil {
		return errors.Join(err, a.reconcile(ctx, repo, provider, id, true))
	}

	// The restore brought the guest up over rules it has no memory of, so the host's go on again now.
	if err := net.Reapply(ctx, id); err != nil {
		err = fmt.Errorf("sandbox %s is running and its network rules were not applied again, so stop it or resume it again: %w", id, err)

		return errors.Join(err, a.reconcile(ctx, repo, provider, id, true))
	}

	if err := a.recordRunning(ctx, repo, provider, id, true); err != nil {
		return err
	}

	return a.print(id)
}
