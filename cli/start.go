package cli

import (
	"context"
	"fmt"

	"github.com/presmihaylov/shard/models"
)

// start runs a stopped sandbox again. Its address, its writable layer and its record all survived
// the stop, so the provider builds the new run over them and the record loses only the old exit.
func (a App) start(ctx context.Context, args []string) error {
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

	sb, err := repo.Get(id)
	if err != nil {
		return err
	}

	if sb.State != models.StateStopped {
		return fmt.Errorf("sandbox %s is %s: start takes a stopped sandbox", id, sb.State)
	}

	if err := provider.Start(ctx, id); err != nil {
		return err
	}

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
		return err
	}

	return a.print(id)
}
