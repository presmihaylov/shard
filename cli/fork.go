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

// fork starts a new sandbox from the snapshot of another, and reads nothing else of the source.
func (a App) fork(ctx context.Context, args []string) (err error) {
	flags := flag.NewFlagSet("shard fork", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	forkName := flags.String("name", "", "a handle every verb takes in place of the id")

	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse the fork flags: %w", err)
	}
	if named(flags) {
		if err := sandboxstate.ValidName(*forkName); err != nil {
			return err
		}
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("fork takes one sandbox id, got %d", flags.NArg())
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

	if err := requireVerb(provider, models.VerbFork); err != nil {
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

	// A pause in flight would replace the snapshot under the restore, and an rm would take it away.
	release, err := repo.HoldShared(ctx, source)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()

	sb, err := repo.Get(source)
	if err != nil {
		return err
	}
	if sb.Snapshot == "" {
		return fmt.Errorf("sandbox %s has no snapshot: pause it first, fork reads what the pause wrote", source)
	}

	var td teardown

	defer func() {
		if err != nil {
			err = errors.Join(err, td.unwind(ctx))
		}
	}()

	// The memory image holds the source's run, so an entrypoint that had exited before the pause has too.
	forked, err := repo.Create(models.Sandbox{
		Name:       *forkName,
		Image:      sb.Image,
		Provider:   provider.Name(),
		State:      models.StateCreated,
		Resources:  sb.Resources,
		Secrets:    slices.Clone(sb.Secrets),
		Policy:     sb.Policy,
		ExitStatus: sb.ExitStatus,
		CreatedAt:  time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	id := forked.ID

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

	// The network lands in the record before the restore, so a fork that fails after it can be given back.
	err = repo.Update(id, func(sb *models.Sandbox) error {
		sb.NetnsPath = netSpec.NetnsPath
		sb.Address = netSpec.Address
		sb.HostInterface = netSpec.HostInterface

		return nil
	})
	if err != nil {
		return err
	}

	// The chain is keyed by the address, which the record holds only now, so the host learns it before the guest runs.
	if sb.Policy != "" {
		if err := net.Reapply(ctx, id); err != nil {
			return err
		}
	}

	td.push(func(ctx context.Context) error { return provider.Remove(ctx, id) })

	spec := models.SandboxSpec{ID: id, Name: *forkName, StateDir: dir, Network: netSpec, Resources: sb.Resources}
	if err := provider.Fork(ctx, sb.Snapshot, spec); err != nil {
		// An interrupt kills the restore process, not what it may already have restored, and only stop
		// ends a sandbox, so an unknown outcome is kept.
		if ctx.Err() != nil {
			td.discard()

			return fmt.Errorf("the fork into sandbox %s was interrupted, so it may be running and it stays on the host: %w", id, err)
		}

		return err
	}

	// The commit point: the fork is live, so nothing below gives anything back.
	td.discard()

	// The restored guest holds no rules of its own, so the host's go on again now.
	if err := net.Reapply(ctx, id); err != nil {
		return errors.Join(err, a.reconcile(ctx, repo, provider, id, true))
	}

	// The id is printed before the record write, so a fork whose record failed is still reachable.
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
