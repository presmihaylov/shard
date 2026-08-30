package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/runspec"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// DefaultInitPath is where make devbox-sync installs the guest supervisor on the box.
const DefaultInitPath = "/usr/local/bin/shard-init"

// InitPathEnv overrides DefaultInitPath. It is a property of the install, so it is no create flag.
const InitPathEnv = "SHARD_INIT_PATH"

// teardownBudget bounds the whole give-back after a failed create, on a context the command's own
// cannot cancel.
const teardownBudget = 30 * time.Second

// maxMemoryMiB is 16 TiB, which is past any host and far below the point where MiB times 2^20 wraps.
const maxMemoryMiB = 1 << 24

// createOptions is one parsed shard create invocation.
type createOptions struct {
	ref  string
	argv []string

	name    string
	env     []string
	workDir string
	user    string

	resources models.Resources
}

func (a App) create(ctx context.Context, args []string) error {
	opts, err := parseCreate(args)
	if err != nil {
		return err
	}

	return a.launch(ctx, a.deps(), opts)
}

// parseCreate splits the flags, the image and the argv. Go's flag stops at the first non-flag
// argument, so the flags must precede the image and what is left is the image plus a literal --
// plus the argv.
func parseCreate(args []string) (createOptions, error) {
	var opts createOptions

	flags := flag.NewFlagSet("shard create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.name, "name", "", "a handle every verb takes in place of the id")
	flags.Var((*envList)(&opts.env), "env", "an environment variable as KEY=VALUE, repeatable")
	flags.StringVar(&opts.workDir, "workdir", "", "the directory the entrypoint starts in")
	flags.StringVar(&opts.user, "user", "", "the user the entrypoint runs as")
	flags.Int64Var(&opts.resources.MemoryMiB, "memory", 0, "the memory bound in MiB, 0 for unbounded")
	flags.IntVar(&opts.resources.VCPUs, "cpus", 0, "the vcpu bound, 0 for unbounded")

	if err := flags.Parse(args); err != nil {
		return createOptions{}, fmt.Errorf("parse the create flags: %w", err)
	}

	// The spelling is checked here, so a name no verb could take back never costs the operator a pull.
	if named(flags) {
		if err := sandboxstate.ValidName(opts.name); err != nil {
			return createOptions{}, err
		}
	}

	// A bound below zero is not a spelling of unbounded, and the substrate would drop it without a word.
	if opts.resources.MemoryMiB < 0 {
		return createOptions{}, fmt.Errorf("--memory is a bound in MiB and cannot be negative, got %d", opts.resources.MemoryMiB)
	}
	// A bound this large overflows the byte count it is turned into, and an overflow reads as unbounded.
	if opts.resources.MemoryMiB > maxMemoryMiB {
		return createOptions{}, fmt.Errorf("--memory is a bound in MiB and no host holds that much, got %d", opts.resources.MemoryMiB)
	}
	if opts.resources.VCPUs < 0 {
		return createOptions{}, fmt.Errorf("--cpus is a bound and cannot be negative, got %d", opts.resources.VCPUs)
	}

	rest := flags.Args()
	if len(rest) == 0 {
		return createOptions{}, errors.New("create takes one image reference, got none")
	}

	opts.ref, rest = rest[0], rest[1:]
	if len(rest) == 0 {
		return opts, nil
	}

	if rest[0] != "--" {
		return createOptions{}, fmt.Errorf("unexpected argument %q: the flags go before the image and the command after --", rest[0])
	}

	opts.argv = rest[1:]
	if len(opts.argv) == 0 {
		return createOptions{}, errors.New("-- takes the command to run, and nothing followed it")
	}

	return opts, nil
}

// named says --name was given, so an explicit empty one is refused rather than read as no name.
func named(flags *flag.FlagSet) bool {
	set := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "name" {
			set = true
		}
	})

	return set
}

// envList refuses anything that is not an assignment, because a merge drops such an entry and
// create would then report success with the variable absent.
type envList []string

func (e *envList) String() string { return strings.Join(*e, ",") }

func (e *envList) Set(value string) error {
	key, _, found := strings.Cut(value, "=")
	if !found {
		return fmt.Errorf("%q is not KEY=VALUE", value)
	}
	if key == "" {
		return fmt.Errorf("%q has no name", value)
	}

	*e = append(*e, value)

	return nil
}

// launch claims the image, the record, the network and the sandbox, then starts the entrypoint and
// prints the id. Every claim before the commit point is pushed onto the teardown stack, because
// half-built state is a bug. Nothing is torn down after it: the sandbox outlives this command.
func (a App) launch(ctx context.Context, d *deps, opts createOptions) (err error) {
	images, err := d.images()
	if err != nil {
		return err
	}

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

	var td teardown

	defer func() {
		if err != nil {
			err = errors.Join(err, td.unwind(ctx))
		}
	}()

	// A pull self-heals its own partial work and sweeps a killed unpack under its own lock, so it
	// claims nothing this command has to give back.
	img, err := a.pullImage(ctx, images, opts.ref)
	if err != nil {
		return err
	}

	id, dir, err := claimRecord(repo, provider, &td, img, opts)
	if err != nil {
		return err
	}

	// Allocate rolls back its own attach only: a failure between the lease claim and the attach leaks
	// the lease file, so the push goes above the call. Release tolerates a lease that was never taken.
	td.push(func(ctx context.Context) error { return net.Release(ctx, id) })

	// The id names the netns, the lease holder and the runsc container, so it must exist first.
	netSpec, err := allocateNetwork(ctx, net, id)
	if err != nil {
		return err
	}

	spec := runspec.Resolve(models.SandboxSpec{
		ID:         id,
		Name:       opts.name,
		RootFS:     img.RootFS,
		StateDir:   dir,
		Entrypoint: opts.argv,
		Env:        opts.env,
		WorkDir:    opts.workDir,
		User:       opts.user,
		Network:    netSpec,
		Resources:  opts.resources,
	}, img.Config)

	// Create rolls back its own mount only, and an interrupt can leave the sandbox process runsc
	// already forked, so the push goes above the call. Remove tolerates an id runsc never held.
	td.push(func(ctx context.Context) error { return provider.Remove(ctx, id) })

	if err := provider.Create(ctx, spec); err != nil {
		return err
	}

	if err := recordCreated(ctx, repo, provider, spec); err != nil {
		return err
	}

	if err := provider.Start(ctx, id); err != nil {
		// An interrupt kills the start process, not what it may already have started, and the substrate
		// cannot tell the two apart. Only stop ends a sandbox, so an unknown outcome is kept.
		if ctx.Err() != nil {
			td.discard()

			return fmt.Errorf("the start of sandbox %s was interrupted, so it may be running and it stays on the host: %w", id, err)
		}

		return err
	}

	// The commit point. The entrypoint is live, so nothing below this line gives anything back: only
	// stop ends a sandbox.
	td.discard()

	// The id is printed before the record write, so a sandbox whose record failed is still reachable.
	if err := a.print(id); err != nil {
		return err
	}

	if err := repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = models.StateRunning

		return nil
	}); err != nil {
		return fmt.Errorf("sandbox %s is running but its record was not updated: %w", id, err)
	}

	return nil
}

// allocateNetwork names the way out, because nothing expires on its own: only an rm frees an address.
func allocateNetwork(ctx context.Context, net sandboxNetwork, id string) (models.NetworkSpec, error) {
	spec, err := net.Allocate(ctx, id)
	if errors.Is(err, network.ErrNoFreeAddress) {
		return models.NetworkSpec{}, fmt.Errorf("%w: every sandbox holds one until it is removed, run shard ls --all and rm the ones you no longer need", err)
	}

	return spec, err
}

func (a App) pullImage(ctx context.Context, images imageService, ref string) (image.Image, error) {
	// A registry that accepts the connection and then stalls would otherwise pin this process forever.
	ctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()

	return images.Pull(ctx, ref)
}

// claimRecord takes the id, which is the only handle every later step is named by.
func claimRecord(repo sandboxRepo, provider models.Provider, td *teardown, img image.Image, opts createOptions) (string, string, error) {
	sb, err := repo.Create(models.Sandbox{
		Name:      opts.name,
		Image:     img.Reference,
		Provider:  provider.Name(),
		State:     models.StateCreated,
		Resources: opts.resources,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return "", "", err
	}

	// Create is atomic, so there is nothing to give back until it returns; a failed Dir still deletes.
	td.push(func(context.Context) error { return repo.Delete(sb.ID) })

	dir, err := repo.Dir(sb.ID)
	if err != nil {
		return "", "", err
	}

	return sb.ID, dir, nil
}

// recordCreated copies what the substrate decided into the record, so a later shard process can
// reach the sandbox without asking the provider again. The state stays created until the start.
func recordCreated(ctx context.Context, repo sandboxRepo, provider models.Provider, spec models.SandboxSpec) error {
	status, err := provider.Status(ctx, spec.ID)
	if err != nil {
		return err
	}

	return repo.Update(spec.ID, func(sb *models.Sandbox) error {
		sb.PID = status.PID
		sb.NetnsPath = spec.Network.NetnsPath
		sb.Address = spec.Network.Address
		sb.HostInterface = spec.Network.HostInterface

		return nil
	})
}

// teardown is what a failed create gives back, in the reverse of the order it was claimed.
type teardown struct {
	steps []func(context.Context) error
}

func (t *teardown) push(step func(context.Context) error) { t.steps = append(t.steps, step) }

// discard is the commit point: what is on the stack now belongs to a sandbox that is live.
func (t *teardown) discard() { t.steps = nil }

// unwind runs the stack on a fresh bounded context, because Ctrl-C cancelled the command's own and
// every call would then fail at once and give nothing back. It stops at the first failure, because a
// step that failed still holds what the steps below it name.
func (t *teardown) unwind(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownBudget)
	defer cancel()

	for i, step := range slices.Backward(t.steps) {
		if err := step(ctx); err != nil {
			return fmt.Errorf("gave back %d of %d claims and stopped, the rest are left on the host: %w",
				len(t.steps)-1-i, len(t.steps), err)
		}
	}

	return nil
}
