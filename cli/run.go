package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/provider/gvisor"
	"github.com/presmihaylov/shard/services/runspec"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// DefaultInitPath is where make devbox-sync installs the guest supervisor on the box.
const DefaultInitPath = "/usr/local/bin/shard-init"

// InterruptedExitCode is what a shell reports for a command a SIGINT ended.
const InterruptedExitCode = 130

// teardownBudget bounds the whole give-back after a failed run, on a context the run's own cannot cancel.
const teardownBudget = 30 * time.Second

// tailInterval paces the log follower, because a file that grows behind our back wakes nobody.
const tailInterval = 100 * time.Millisecond

// ExitError carries the code shard itself must exit with. Only run returns one today.
type ExitError struct {
	Code int
}

func (e ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// runOptions is one parsed shard run invocation.
type runOptions struct {
	ref  string
	argv []string

	env     []string
	workDir string
	user    string

	resources models.Resources
	initPath  string
}

// imageService is the part of image.Service that run drives.
type imageService interface {
	Pull(ctx context.Context, ref string) (image.Image, error)
}

// sandboxRepo is the part of sandboxstate.Repository that run drives.
type sandboxRepo interface {
	Create(sb models.Sandbox) (models.Sandbox, error)
	Update(id string, mutate func(*models.Sandbox) error) error
	Delete(id string) error
	Dir(id string) (string, error)
}

// sandboxNetwork is the part of network.Service that run drives.
type sandboxNetwork interface {
	Allocate(ctx context.Context, id string) (models.NetworkSpec, error)
	Release(ctx context.Context, id string) error
}

// runDeps is every layer run wires together. A test replaces the factory, because each real part
// needs Linux and root.
type runDeps struct {
	images   imageService
	repo     sandboxRepo
	net      sandboxNetwork
	provider models.Provider
}

func (a App) run(ctx context.Context, args []string) error {
	opts, err := parseRun(args)
	if err != nil {
		return err
	}

	build := a.newRunDeps
	if build == nil {
		build = defaultRunDeps
	}

	deps, err := build(a, opts)
	if err != nil {
		return err
	}

	return a.launch(ctx, deps, opts)
}

// parseRun splits the flags, the image and the argv. Go's flag stops at the first non-flag argument,
// so the flags must precede the image and what is left is the image plus a literal -- plus the argv.
func parseRun(args []string) (runOptions, error) {
	opts := runOptions{initPath: DefaultInitPath}

	flags := flag.NewFlagSet("shard run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Var((*stringList)(&opts.env), "env", "an environment variable as KEY=VALUE, repeatable")
	flags.StringVar(&opts.workDir, "workdir", "", "the directory the entrypoint starts in")
	flags.StringVar(&opts.user, "user", "", "the user the entrypoint runs as")
	flags.Int64Var(&opts.resources.MemoryMiB, "memory", 0, "the memory bound in MiB, 0 for unbounded")
	flags.IntVar(&opts.resources.VCPUs, "cpus", 0, "the vcpu bound, 0 for unbounded")
	flags.StringVar(&opts.initPath, "shard-init", opts.initPath, "the host path of the guest supervisor")

	if err := flags.Parse(args); err != nil {
		return runOptions{}, fmt.Errorf("parse the run flags: %w", err)
	}

	rest := flags.Args()
	if len(rest) == 0 {
		return runOptions{}, errors.New("run takes one image reference, got none")
	}

	opts.ref, rest = rest[0], rest[1:]
	if len(rest) == 0 {
		return opts, nil
	}

	if rest[0] != "--" {
		return runOptions{}, fmt.Errorf("unexpected argument %q: the flags go before the image and the command after --", rest[0])
	}

	opts.argv = rest[1:]
	if len(opts.argv) == 0 {
		return runOptions{}, errors.New("-- takes the command to run, and nothing followed it")
	}

	return opts, nil
}

// defaultRunDeps builds the real layers. Every one of them refuses off Linux, which is why the
// factory is a field the wiring test replaces rather than something run calls directly.
func defaultRunDeps(a App, opts runOptions) (runDeps, error) {
	images, err := a.images()
	if err != nil {
		return runDeps{}, err
	}

	repo, err := sandboxstate.New(a.Root)
	if err != nil {
		return runDeps{}, err
	}

	manager, err := netns.New()
	if err != nil {
		return runDeps{}, err
	}

	net, err := network.New(network.Config{Root: a.Root}, manager)
	if err != nil {
		return runDeps{}, err
	}

	// The mode is fixed on the runner, and a sandbox with an allocated netns needs netstack over it.
	runner, err := runsc.New(filepath.Join(a.Root, "runsc"), runsc.WithNetwork(runsc.NetworkSandbox))
	if err != nil {
		return runDeps{}, err
	}

	bundles, err := bundle.New(opts.initPath)
	if err != nil {
		return runDeps{}, err
	}

	provider, err := gvisor.New(runner, bundles, repo.Dir)
	if err != nil {
		return runDeps{}, err
	}

	return runDeps{images: images, repo: repo, net: net, provider: provider}, nil
}

// launch claims the image, the record, the network and the sandbox, then streams the guest's output
// until the entrypoint exits. Every claim before the commit point is pushed onto the teardown stack,
// because half-built state is a bug. Nothing is torn down after it: the sandbox outlives this command.
func (a App) launch(ctx context.Context, deps runDeps, opts runOptions) (err error) {
	var td teardown

	defer func() {
		if err != nil {
			err = errors.Join(err, td.unwind(ctx))
		}
	}()

	// A pull self-heals its own partial work and the next image.New sweeps a killed unpack, so it
	// claims nothing this command has to give back.
	img, err := a.pullImage(ctx, deps, opts.ref)
	if err != nil {
		return err
	}

	id, dir, err := claimRecord(deps, &td, img, opts)
	if err != nil {
		return err
	}

	// The id names the netns, the lease holder and the runsc container, so it must exist first.
	netSpec, err := deps.net.Allocate(ctx, id)
	if err != nil {
		return err
	}

	// Allocate rolls back its own attach only: a failure between the lease claim and the attach
	// leaks the lease file, so the caller owns the release.
	td.push(func(ctx context.Context) error { return deps.net.Release(ctx, id) })

	spec := runspec.Resolve(models.SandboxSpec{
		ID:         id,
		RootFS:     img.RootFS,
		StateDir:   dir,
		Entrypoint: opts.argv,
		Env:        opts.env,
		WorkDir:    opts.workDir,
		User:       opts.user,
		Network:    netSpec,
		Resources:  opts.resources,
	}, img.Config)

	if err := deps.provider.Create(ctx, spec); err != nil {
		return err
	}

	// Remove unmounts, and that must happen before the repository removes the directory under it.
	td.push(func(ctx context.Context) error { return deps.provider.Remove(ctx, id) })

	if err := recordCreated(ctx, deps, spec); err != nil {
		return err
	}

	logPath, err := deps.provider.LogPath(id)
	if err != nil {
		return err
	}

	// The offset is read before the start, because the log is append-mode and a state directory that
	// already ran holds the previous run's output.
	offset, err := logOffset(logPath)
	if err != nil {
		return err
	}

	if err := deps.provider.Start(ctx, id); err != nil {
		return err
	}

	if err := deps.repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = models.StateRunning

		return nil
	}); err != nil {
		// The sandbox is live, so it is left alone: deleting it would break the keep-alive rule.
		return fmt.Errorf("sandbox %s is running but its record was not updated: %w", id, err)
	}

	// The commit point. The sandbox now outlives this command, so nothing is given back any more.
	td.discard()
	a.note("sandbox " + id)

	return a.attach(ctx, deps, id, logPath, offset)
}

func (a App) pullImage(ctx context.Context, deps runDeps, ref string) (image.Image, error) {
	// A registry that accepts the connection and then stalls would otherwise pin this process forever.
	ctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()

	return deps.images.Pull(ctx, ref)
}

// claimRecord takes the id, which is the only handle every later step is named by.
func claimRecord(deps runDeps, td *teardown, img image.Image, opts runOptions) (string, string, error) {
	sb, err := deps.repo.Create(models.Sandbox{
		Image:     img.Reference,
		Provider:  deps.provider.Name(),
		State:     models.StateCreated,
		Resources: opts.resources,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return "", "", err
	}

	td.push(func(context.Context) error { return deps.repo.Delete(sb.ID) })

	dir, err := deps.repo.Dir(sb.ID)
	if err != nil {
		return "", "", err
	}

	return sb.ID, dir, nil
}

// recordCreated copies what the substrate decided into the record, so a later shard process can
// reach the sandbox without asking the provider again. The state stays created until the start.
func recordCreated(ctx context.Context, deps runDeps, spec models.SandboxSpec) error {
	status, err := deps.provider.Status(ctx, spec.ID)
	if err != nil {
		return err
	}

	return deps.repo.Update(spec.ID, func(sb *models.Sandbox) error {
		sb.Name = spec.Name
		sb.PID = status.PID
		sb.NetnsPath = spec.Network.NetnsPath
		sb.Address = spec.Network.Address
		sb.HostInterface = spec.Network.HostInterface

		return nil
	})
}

// attach streams the guest's output and waits for the entrypoint. It is the only half of run that
// may be interrupted without anything being given back.
func (a App) attach(ctx context.Context, deps runDeps, id, path string, offset int64) (err error) {
	log, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { err = errors.Join(err, log.Close()) }()

	if _, err := log.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s: %w", path, err)
	}

	exited := make(chan struct{})
	tailed := make(chan error, 1)

	go func() { tailed <- tail(ctx, a.Out, log, exited) }()

	status, waitErr := deps.provider.Wait(ctx, id)
	close(exited)

	// A run that could not show the guest's output failed, whatever the entrypoint went on to do.
	if tailErr := <-tailed; tailErr != nil {
		return errors.Join(tailErr, waitErr)
	}

	if waitErr != nil {
		// Ctrl-C detaches. Stopping the sandbox here would be the one on-exit behaviour shard forbids.
		if ctx.Err() != nil {
			a.note(fmt.Sprintf("sandbox %s is still running; use shard stop %s", id, id))

			return ExitError{Code: InterruptedExitCode}
		}

		return waitErr
	}

	// The state stays running: the entrypoint exiting is not a transition.
	if err := deps.repo.Update(id, func(sb *models.Sandbox) error {
		sb.ExitStatus = &status

		return nil
	}); err != nil {
		return err
	}

	return ExitError{Code: status.Code}
}

// logOffset is where the guest's output for this run begins. A provider that writes the file only
// once the guest does has nothing there yet, and that is the same as an empty one.
func logOffset(path string) (int64, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}

	return info.Size(), nil
}

// tail copies the log to w until exited is closed, then once more, because the entrypoint may have
// written between the last copy and its exit. runsc interleaves stdout and stderr into this one file.
func tail(ctx context.Context, w io.Writer, log io.Reader, exited <-chan struct{}) error {
	for {
		n, err := io.Copy(w, log)
		if err != nil {
			return fmt.Errorf("stream the sandbox output: %w", err)
		}
		if n > 0 {
			continue
		}

		select {
		case <-exited:
			if _, err := io.Copy(w, log); err != nil {
				return fmt.Errorf("stream the sandbox output: %w", err)
			}

			return nil
		case <-ctx.Done():
			// An interrupt detaches the viewer, and that is not a failure.
			return nil
		case <-time.After(tailInterval):
		}
	}
}

// teardown is what a failed run gives back, in the reverse of the order it was claimed.
type teardown struct {
	steps []func(context.Context) error
}

func (t *teardown) push(step func(context.Context) error) { t.steps = append(t.steps, step) }

// discard is the commit point: what is on the stack now belongs to a sandbox that is live.
func (t *teardown) discard() { t.steps = nil }

// unwind runs the stack on a fresh bounded context, because Ctrl-C cancelled the run's own and every
// call would then fail at once and give nothing back. No error is swallowed.
func (t *teardown) unwind(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownBudget)
	defer cancel()

	var err error
	for i := len(t.steps) - 1; i >= 0; i-- {
		err = errors.Join(err, t.steps[i](ctx))
	}

	return err
}
