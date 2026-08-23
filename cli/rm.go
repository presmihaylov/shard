package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/presmihaylov/shard/services/sandboxstate"
)

// rmOptions is one parsed shard rm invocation.
type rmOptions struct {
	id    string
	force bool
	grace time.Duration
}

func (a App) remove(ctx context.Context, args []string) error {
	opts, err := parseRm(args)
	if err != nil {
		return err
	}

	deps, err := a.lifecycle()
	if err != nil {
		return err
	}

	// The record dies last below, so an id with no record has nothing else left on the host either.
	_, err = deps.repo.Get(opts.id)
	if errors.Is(err, sandboxstate.ErrNotFound) {
		a.warn(fmt.Sprintf("sandbox %s does not exist, so there is nothing to remove", opts.id))

		return nil
	}
	if err != nil {
		return err
	}

	if err := a.endIfAlive(ctx, deps, opts); err != nil {
		return err
	}

	if err := free(ctx, deps, opts.id); err != nil {
		return err
	}

	return a.print(opts.id)
}

// endIfAlive refuses a sandbox that is still up, because rm frees the writable layer a stop keeps.
// --force is the shorthand for the stop the operator would otherwise type first.
func (a App) endIfAlive(ctx context.Context, deps lifecycleDeps, opts rmOptions) error {
	status, err := deps.provider.Status(ctx, opts.id)
	if err != nil {
		return err
	}
	if !status.Alive() {
		return nil
	}

	if !opts.force {
		return fmt.Errorf("sandbox %s is %s: stop it first with shard stop %s, or pass --force", opts.id, status.State, opts.id)
	}

	return a.stopSandbox(ctx, deps, opts.id, opts.grace)
}

// holding is one of the things a stopped sandbox still holds on the host.
type holding struct {
	what string
	free func() error
}

// free gives back everything a stop kept, and stops at the first failure: a step that failed still
// holds what the steps below it name. The record goes last, because it is the only handle by which
// the mount and the namespace can be found again.
func free(ctx context.Context, deps lifecycleDeps, id string) error {
	held := []holding{
		{"runsc state and rootfs mount", func() error { return deps.provider.Remove(ctx, id) }},
		{"netns, veth and address lease", func() error { return deps.net.Release(ctx, id) }},
		{"record and state directory", func() error { return deps.repo.Delete(id) }},
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
