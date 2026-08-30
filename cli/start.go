package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/presmihaylov/shard/models"
)

// start runs a stopped sandbox again. Its address, its writable layer and its record all survived
// the stop, so the provider builds the new run over them and the record loses only the old exit.
func (a App) start(ctx context.Context, args []string) (err error) {
	if len(args) != 1 {
		return fmt.Errorf("start takes one sandbox id, got %d", len(args))
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

	// Two starts of one sandbox would each build the netns; the second waits and then sees it running.
	release, err := repo.Hold(ctx, id)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()

	sb, err := repo.Get(id)
	if err != nil {
		return err
	}

	if sb.State != models.StateStopped {
		return fmt.Errorf("sandbox %s is %s: start takes a stopped sandbox", id, sb.State)
	}

	net, err := d.net()
	if err != nil {
		return err
	}

	// The lease survived the stop, so this hands back the same address over a namespace built again.
	if _, err := net.Allocate(ctx, id); err != nil {
		return err
	}

	if err := provider.Start(ctx, id); err != nil {
		return errors.Join(err, a.reconcile(ctx, repo, provider, id))
	}

	if err := a.recordRunning(ctx, repo, provider, id); err != nil {
		return err
	}

	return a.print(id)
}

// reconcile is for a start that failed: the substrate may hold a live sandbox anyway, and only stop
// ends one, so the record must say so rather than call it stopped.
func (a App) reconcile(ctx context.Context, repo sandboxRepo, provider models.Provider, id string) error {
	status, err := provider.Status(ctx, id)
	if err != nil {
		return err
	}
	if !status.Alive() {
		return nil
	}

	if err := a.recordRunning(ctx, repo, provider, id); err != nil {
		return err
	}

	return fmt.Errorf("sandbox %s may be running and it stays on the host", id)
}

func (a App) recordRunning(ctx context.Context, repo sandboxRepo, provider models.Provider, id string) error {
	status, err := provider.Status(ctx, id)
	if err != nil {
		return err
	}

	err = repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = models.StateRunning
		sb.PID = status.PID
		// The old exit is what the previous run did, and this run has not ended.
		sb.ExitStatus = nil

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s is running but its record was not updated: %w", id, err)
	}

	return nil
}
