package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// clone starts a new sandbox over a copy of the files another one kept, and runs its entrypoint from
// the beginning. It takes no memory: that is fork, which reads a snapshot.
func (a App) clone(ctx context.Context, args []string) (err error) {
	flags := flag.NewFlagSet("shard clone", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	cloneName := flags.String("name", "", "a handle every verb takes in place of the id")

	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse the clone flags: %w", err)
	}
	if named(flags) {
		if err := sandboxstate.ValidName(*cloneName); err != nil {
			return err
		}
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("clone takes one sandbox id, got %d", flags.NArg())
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

	net, err := d.net()
	if err != nil {
		return err
	}

	source, err := repo.Resolve(flags.Arg(0))
	if err != nil {
		return err
	}

	// A start in flight would write under the copy, and an rm would take the layer away.
	release, err := repo.HoldShared(ctx, source)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()

	sb, err := repo.Get(source)
	if err != nil {
		return err
	}
	if sb.State != models.StateStopped && sb.State != models.StatePaused {
		return fmt.Errorf("sandbox %s is %s: stop it first, clone copies what a stop kept", source, sb.State)
	}

	var td teardown

	defer func() {
		if err != nil {
			err = errors.Join(err, td.unwind(ctx))
		}
	}()

	// The entrypoint runs from the beginning, so the source's exit is not the clone's.
	cloned, err := repo.Create(models.Sandbox{
		Name:      *cloneName,
		Image:     sb.Image,
		Provider:  provider.Name(),
		State:     models.StateCreated,
		Resources: sb.Resources,
		Secrets:   slices.Clone(sb.Secrets),
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	id := cloned.ID

	td.push(func(context.Context) error { return repo.Delete(id) })

	dir, err := repo.Dir(id)
	if err != nil {
		return err
	}

	td.push(func(ctx context.Context) error { return net.Release(ctx, id) })

	netSpec, err := allocateNetwork(ctx, net, id)
	if err != nil {
		return err
	}

	// The network lands in the record first, so a clone that fails after it can be given back.
	err = repo.Update(id, func(sb *models.Sandbox) error {
		sb.NetnsPath = netSpec.NetnsPath
		sb.Address = netSpec.Address
		sb.HostInterface = netSpec.HostInterface

		return nil
	})
	if err != nil {
		return err
	}

	td.push(func(ctx context.Context) error { return provider.Remove(ctx, id) })

	spec := models.SandboxSpec{ID: id, Name: *cloneName, StateDir: dir, Network: netSpec, Resources: sb.Resources}
	if err := provider.Clone(ctx, source, spec); err != nil {
		// An interrupt kills the start process and not what it started, and only stop ends a sandbox.
		if ctx.Err() != nil {
			td.discard()

			return fmt.Errorf("the clone into sandbox %s was interrupted, so it may have started and it stays on the host: run shard rm %s: %w", id, id, err)
		}

		return err
	}

	// The commit point: the clone is live, so nothing below gives anything back.
	td.discard()

	// The id is printed before the record write, so a clone whose record failed is still reachable.
	if err := a.print(id); err != nil {
		return err
	}

	status, err := provider.Status(ctx, id)
	if err != nil {
		return err
	}

	err = repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = models.StateRunning
		sb.PID = status.PID

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s is running but its record was not updated: %w", id, err)
	}

	return nil
}
