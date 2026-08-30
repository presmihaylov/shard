package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// rmOptions is one parsed shard rm invocation.
type rmOptions struct {
	id    string
	force bool
	grace time.Duration
}

func (a App) remove(ctx context.Context, args []string) (err error) {
	opts, err := parseRm(args)
	if err != nil {
		return err
	}

	d := a.deps()

	repo, err := d.repo()
	if err != nil {
		return err
	}

	net, err := d.net()
	if err != nil {
		return err
	}

	provider, err := d.provider()
	if err != nil {
		return err
	}

	// The warning below names what the operator typed, which is a name that resolved to nothing.
	ref := opts.id

	opts.id, err = repo.Resolve(ref)
	if err != nil {
		return err
	}

	// The record dies last below, so an id with no record has nothing else left on the host either,
	// and an rm that waited on another rm finds the same nothing.
	release, err := repo.Hold(ctx, opts.id)
	if errors.Is(err, sandboxstate.ErrNotFound) {
		a.warn(fmt.Sprintf("sandbox %s does not exist, so there is nothing to remove", ref))

		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()

	if err := a.endIfAlive(ctx, repo, provider, opts); err != nil {
		return err
	}

	if err := free(ctx, repo, net, provider, opts.id); err != nil {
		return err
	}

	if err := dropSubstrateRoot(d); err != nil {
		return fmt.Errorf("sandbox %s is removed, but %w", opts.id, err)
	}

	return a.print(opts.id)
}

// endIfAlive refuses a sandbox that is still up, because rm frees the writable layer a stop keeps.
// --force is the shorthand for the stop the operator would otherwise type first.
func (a App) endIfAlive(ctx context.Context, repo sandboxRepo, provider models.Provider, opts rmOptions) error {
	status, err := provider.Status(ctx, opts.id)
	if err != nil {
		return err
	}
	if !status.Alive() {
		return nil
	}

	if !opts.force {
		return fmt.Errorf("sandbox %s is %s: stop it first with shard stop %s, or pass --force", opts.id, status.State, opts.id)
	}

	return a.stopSandbox(ctx, repo, provider, opts.id, opts.grace)
}

// holding is one of the things a stopped sandbox still holds on the host.
type holding struct {
	what string
	free func() error
}

// free gives back everything a stop kept, and stops at the first failure: a step that failed still
// holds what the steps below it name. The record goes last, because it is the only handle by which
// the mount and the namespace can be found again.
func free(ctx context.Context, repo sandboxRepo, net sandboxNetwork, provider models.Provider, id string) error {
	held := []holding{
		{"runsc state and rootfs mount", func() error { return provider.Remove(ctx, id) }},
		{"netns, veth and address lease", func() error { return net.Release(ctx, id) }},
		{"record and state directory", func() error { return repo.Delete(id) }},
	}

	for i, h := range held {
		err := h.free()
		if err == nil {
			continue
		}

		left := make([]string, 0, len(held)-i)
		for _, rest := range held[i:] {
			left = append(left, rest.what)
		}

		return fmt.Errorf("remove sandbox %s: %w: its %s are left on the host", id, err, strings.Join(left, ", its "))
	}

	return nil
}

// dropSubstrateRoot gives back what the substrate keeps for itself once no sandbox is left to use
// it. An operator otherwise meets it as an rm -rf of the root that fails with EBUSY.
// A create that runs beside this one is no reason to keep it: runsc binds the mount again on its
// next create, and a live sandbox does not need this mount to stay up.
func dropSubstrateRoot(d *deps) error {
	repo, err := d.repo()
	if err != nil {
		return err
	}

	left, err := repo.List()
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return nil
	}

	sub, err := d.substrate()
	if err != nil {
		return err
	}

	return sub.DropNullNetns()
}

func parseRm(args []string) (rmOptions, error) {
	var opts rmOptions

	flags := flag.NewFlagSet("shard rm", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&opts.force, "force", false, "stop the sandbox first if it is still up")
	flags.DurationVar(&opts.grace, "time", DefaultStopGrace, "how long --force gives the entrypoint before it is killed")

	if err := flags.Parse(args); err != nil {
		return rmOptions{}, fmt.Errorf("parse the rm flags: %w", err)
	}

	if opts.grace < 0 {
		return rmOptions{}, fmt.Errorf("--time is how long the entrypoint gets and cannot be negative, got %s", opts.grace)
	}

	rest := flags.Args()
	if len(rest) != 1 {
		return rmOptions{}, fmt.Errorf("rm takes one sandbox id, got %d", len(rest))
	}

	opts.id = rest[0]

	return opts, nil
}
