// Package gvisor runs sandboxes on gVisor by driving bare runsc.
package gvisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/runspec"
)

// Name is the substrate, as --provider and the record name it.
const Name = "gvisor"

// logFile holds the guest's stdout and stderr, interleaved the way a terminal would show them.
const logFile = "output.log"

const (
	// pollInterval paces every wait here. runsc state is a socket round trip, so it is not free.
	pollInterval = 100 * time.Millisecond
	// killGrace bounds the wait after SIGKILL, which the sentry cannot refuse.
	killGrace = 10 * time.Second
	// startGrace bounds the wait for the supervisor's handshake, which it writes as soon as it forks.
	startGrace = 30 * time.Second
	// captureGrace bounds how long a live fork holds its source frozen, so a wedged checkpoint or copy cannot keep it so.
	captureGrace = 10 * time.Minute
)

// diagnosticTail bounds what a failed start quotes back from the sandbox's own output.
const diagnosticTail = 4 << 10

// checkpointFile is the one file every runsc checkpoint writes, so its absence says there is no checkpoint.
const checkpointFile = "checkpoint.img"

// forkFrozenFile marks a source that a live fork froze, so a daemon cut before the thaw resumes it on its next read (SHARD-457).
const forkFrozenFile = "fork-frozen"

// captureDir holds the capture a live fork restores from, in the fork's own state directory, so it goes with the fork and is never a snapshot (SHARD-457).
const captureDir = "capture"

// StateDirs answers where a sandbox's directory is. sandboxstate.Repository.Dir is what shard passes:
// every verb below takes an id, and shard runs no daemon that could remember the path from Create.
type StateDirs func(id string) (string, error)

var _ models.Provider = (*Provider)(nil)

// runscCtl is the runsc surface the provider drives. The field is this interface, not the concrete runner, so the teardown is unit-testable. It holds no force delete: safeDelete sweeps the cgroup and forgets the state (SHARD-440).
type runscCtl interface {
	Create(ctx context.Context, id string, opts runsc.CreateOptions) error
	Exec(ctx context.Context, id string, opts runsc.ExecOptions) (int, error)
	Signal(ctx context.Context, id string, pid int, signal string) error
	Start(ctx context.Context, id string) error
	Pause(ctx context.Context, id string) error
	Resume(ctx context.Context, id string) error
	Checkpoint(ctx context.Context, id, dir string) error
	CheckpointRunning(ctx context.Context, id, dir string) error
	Restore(ctx context.Context, id string, opts runsc.RestoreOptions) error
	RestoreArgs(id string, opts runsc.RestoreOptions) []string
	Kill(ctx context.Context, id, signal string, all bool) error
	State(ctx context.Context, id string) (runsc.State, error)
	Forget(id string) error
	Executable() string
	DropNullNetns() error
}

// Provider implements models.Provider on gVisor.
type Provider struct {
	runsc   runscCtl
	bundles *bundle.Service
	dirs    StateDirs
	caps    models.Capabilities
	// cgroupRoot is the host cgroup v2 mount. A test points it at a directory it can write.
	cgroupRoot string
	// procRoot is where the kernel publishes a process's command line and state. A test points it at a directory it wrote.
	procRoot string
	// killPinned is the SIGKILL a teardown sends, delivered only if still holds once the process is pinned. A test records the pid instead, because there is no process to kill.
	killPinned func(pid int, still func() (bool, error)) error
	// capturing holds each source a live fork of this process has frozen, which a read must not thaw under it; a cut daemon leaves none, so the next one thaws by the mark.
	capturing sync.Map
	// forkMu makes a capture's mark and a read's thaw one step each, so a read never thaws a freeze that began after it looked.
	forkMu sync.Mutex
}

func New(runner *runsc.Runner, bundles *bundle.Service, dirs StateDirs) (*Provider, error) {
	if runner == nil || bundles == nil || dirs == nil {
		return nil, errors.New("the gvisor provider needs a runsc runner, a bundle service and a state directory lookup")
	}

	// Capabilities is fixed once here, so it needs no context and cannot fail.
	caps := models.Capabilities{Pause: true, Resume: true, Fork: true}

	return &Provider{runsc: runner, bundles: bundles, dirs: dirs, caps: caps, cgroupRoot: cgroup.Root, procRoot: "/proc", killPinned: pidfdKill}, nil
}

func (p *Provider) Name() string { return Name }

// CheckResources refuses a memory bound under the sentry's own cost, which kills the create with nothing shard can read back.
func (p *Provider) CheckResources(res models.Resources) error {
	if res.MemoryMiB > 0 && res.MemoryMiB < MinimumMemoryMiB {
		return fmt.Errorf("%s needs at least %d MiB of memory, got %d: the sentry itself costs about 30 MiB", Name, MinimumMemoryMiB, res.MemoryMiB)
	}

	return nil
}

func (p *Provider) Capabilities() models.Capabilities { return p.caps }

// ReleaseRoot unmounts the null netns runsc keeps under its root, which no sandbox teardown drops.
func (p *Provider) ReleaseRoot() error { return p.runsc.DropNullNetns() }

// Create builds the bundle, stacks the writable layer over the image and prepares the container.
func (p *Provider) Create(ctx context.Context, spec models.SandboxSpec) error {
	// Create checks its spec again, so every path to it is held to the same rule.
	if err := p.CheckResources(spec.Resources); err != nil {
		return fmt.Errorf("sandbox %s: %w", spec.ID, err)
	}

	// A live id must not be re-created: the rollback below would unmount the rootfs the first one runs on.
	status, err := p.Status(ctx, spec.ID)
	if err != nil {
		return err
	}
	if status.Alive() {
		return fmt.Errorf("sandbox %s already exists on %s and is %s", spec.ID, Name, status.State)
	}

	// A rootfs that stands while runsc holds nothing may still be a live sandbox's, so never build over it.
	existing, err := bundle.Open(spec.StateDir)
	if err != nil {
		return err
	}
	if err := orphaned(existing, spec.ID, status.Exists); err != nil {
		return err
	}

	// Build writes the layers, so the disk they live on comes up first.
	if err := existing.Provision(spec.Resources); err != nil {
		return err
	}

	b, err := p.bundles.Build(spec)
	if err != nil {
		return errors.Join(err, existing.Unmount())
	}

	if err := b.Mount(spec.RootFS); err != nil {
		return errors.Join(err, b.Unmount())
	}

	if err := p.create(ctx, spec, b); err != nil {
		// A half-created sandbox must not leave a mount behind, because nothing else knows to drop it.
		return errors.Join(err, b.Unmount())
	}

	return nil
}

func (p *Provider) create(ctx context.Context, spec models.SandboxSpec, b bundle.Bundle) error {
	// A fresh create must not inherit the old exit, readiness, restart count, or spec-change mark.
	if err := b.ClearRun(); err != nil {
		return err
	}

	return p.bringUp(ctx, spec, b.ExitFile, func(out, exit *os.File) error {
		return p.runsc.Create(ctx, spec.ID, runsc.CreateOptions{Bundle: b.Dir, Stdout: out, Stderr: out, Stdin: exit})
	})
}

// bringUp runs the runsc verb that forks the sandbox process, over the log and exit channel it inherits
// and inside the cgroup shard owns, and then moves the host bounds where create and restore both need them.
func (p *Provider) bringUp(ctx context.Context, spec models.SandboxSpec, exitFile string, up func(out, exit *os.File) error) (err error) {
	out, err := openLog(filepath.Join(spec.StateDir, logFile))
	if err != nil {
		return err
	}
	// The sandbox keeps its own copy of the fd, so closing ours does not cut the guest's output off.
	defer func() { err = errors.Join(err, out.Close()) }()

	// shard-init reports the entrypoint exit on its fd 0, the write end the host holds here: create
	// cleared the stale file first, and fork or restore appends, so a carried or paused record survives.
	exit, err := os.OpenFile(exitFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open the exit channel %s: %w", exitFile, err)
	}
	defer func() { err = errors.Join(err, exit.Close()) }()

	// A teardown rmdirs the sandbox's own cgroup, so its parent must exist and be shard's, never the host cgroup root.
	if err := cgroup.Ensure(filepath.Join(p.cgroupRoot, bundle.CgroupParent)); err != nil {
		return err
	}

	if err := up(out, exit); err != nil {
		if ctx.Err() == nil {
			return err
		}
		// A cancelled bring-up may have forked the sandbox into the cgroup; sweep it, then drop the state only once the sweep clears it (SHARD-440).
		return errors.Join(err, p.safeDelete(context.WithoutCancel(ctx), spec.ID))
	}

	if err := boundMemory(p.cgroupRoot, spec); err != nil {
		// The caller drops the rootfs mount, so a sandbox left created would run on a mount that is gone.
		return errors.Join(err, p.safeDelete(ctx, spec.ID))
	}

	if err := boundPids(p.cgroupRoot, spec.ID); err != nil {
		return errors.Join(err, p.safeDelete(ctx, spec.ID))
	}

	return nil
}

// boundMemory moves the host bounds off the bound the guest sees. runsc has already given the sentry
// the operator's number as its budget, and the host cgroup must sit above it, because that one cgroup
// also charges the sentry's own working set.
func boundMemory(root string, spec models.SandboxSpec) error {
	bound := bundle.MemoryBound(spec.Resources)
	if bound == 0 {
		return nil
	}

	dir := cgroupDir(root, spec.ID)

	applied, err := cgroup.MemoryMax(dir)
	if err != nil {
		return fmt.Errorf("read back the memory bound of sandbox %s: %w", spec.ID, err)
	}

	// runsc applies nothing at all when the cgroup is already there, and an unbounded sandbox is a
	// downgrade, so anything but the number the spec asked for ends the create.
	if applied != bound {
		return fmt.Errorf("sandbox %s asked runsc for a %d byte memory bound on %s, which holds %d, where -1 is no bound at all",
			spec.ID, bound, filepath.Join(dir, "memory.max"), applied)
	}

	if err := cgroup.SetMemoryMax(dir, MemoryCeiling(spec.Resources)); err != nil {
		return fmt.Errorf("raise the memory ceiling of sandbox %s: %w", spec.ID, err)
	}

	if err := cgroup.SetMemoryHigh(dir, MemoryThrottle(spec.Resources)); err != nil {
		return fmt.Errorf("throttle the memory of sandbox %s: %w", spec.ID, err)
	}

	// Guest memory is sentry shmem, and shmem is swap-backed, so on a host with swap the throttle
	// reclaims instead of holding and stops being the wall the ceiling above it depends on.
	if err := cgroup.SetMemorySwapMax(dir, 0); err != nil {
		return fmt.Errorf("pin the swap of sandbox %s to none: %w", spec.ID, err)
	}

	// Guest memory sits in systrap stubs, so the kernel alone would take one stub and leave the sentry.
	if err := cgroup.SetOOMGroup(dir); err != nil {
		return fmt.Errorf("group the OOM kill of sandbox %s: %w", spec.ID, err)
	}

	return nil
}

// boundPids caps the host cgroup on every launch, so a config.json written before pids were bounded is capped too.
func boundPids(root, id string) error {
	if err := cgroup.SetPidsMax(cgroupDir(root, id), bundle.PidsMax); err != nil {
		return fmt.Errorf("bound the pids of sandbox %s: %w", id, err)
	}

	return nil
}

// cgroupDir is the host side of the path the bundle names.
func cgroupDir(root, id string) string {
	return filepath.Join(root, bundle.CgroupsPath(id))
}

// Start runs the entrypoint. runsc never starts a stopped container again, so a stopped sandbox is
// re-created first over the writable layer its state directory kept.
// It returns only once the supervisor says the entrypoint forked, because runsc start unblocks the
// task and reads nothing back: a broken entrypoint would otherwise report as a started sandbox.
func (p *Provider) Start(ctx context.Context, id string) error {
	dir, err := p.dirs(id)
	if err != nil {
		return err
	}

	b, err := bundle.Open(dir)
	if err != nil {
		return err
	}

	status, err := p.Status(ctx, id)
	if err != nil {
		return err
	}

	changed, err := b.Changed()
	if err != nil {
		return err
	}

	// A created container holds the config.json of its create, so a grant since then reaches the guest only through a new one.
	if !status.Alive() || (status.State == models.StateCreated && changed) {
		if err := p.recreate(ctx, id, dir, b, status.Exists); err != nil {
			return err
		}
	}

	if err := p.runsc.Start(ctx, id); err != nil {
		return err
	}

	return p.awaitStarted(ctx, id, b)
}

// recreate gives a stopped or changed created sandbox a fresh runtime over its preserved bundle.
func (p *Provider) recreate(ctx context.Context, id, dir string, b bundle.Bundle, held bool) error {
	spec, err := p.reclaim(ctx, id, dir, b, held)
	if err != nil {
		return err
	}

	if err := p.create(ctx, spec, b); err != nil {
		return errors.Join(err, b.Unmount())
	}

	return nil
}

// reclaim readies a state directory runsc holds nothing live in for a new sandbox process: the old
// container and its cgroup go, and the writable layer is mounted again over the image it records.
func (p *Provider) reclaim(ctx context.Context, id, dir string, b bundle.Bundle, held bool) (models.SandboxSpec, error) {
	if err := orphaned(b, id, held); err != nil {
		return models.SandboxSpec{}, err
	}

	// Everything the new run needs is checked before the old container goes, so a refusal costs nothing.
	rt, err := imageOf(b, id)
	if err != nil {
		return models.SandboxSpec{}, err
	}

	if held {
		if err := p.safeDelete(ctx, id); err != nil {
			return models.SandboxSpec{}, err
		}
	}

	// A cgroup runsc left behind would make the create refuse the bound it could not apply.
	if err := cgroup.Remove(cgroupDir(p.cgroupRoot, id)); err != nil {
		return models.SandboxSpec{}, fmt.Errorf("sweep the cgroup of sandbox %s: %w", id, err)
	}

	if err := b.Mount(rt.RootFS); err != nil {
		return models.SandboxSpec{}, err
	}

	return models.SandboxSpec{ID: id, StateDir: dir, Resources: rt.Resources}, nil
}

// awaitStarted watches for the handshake and for the sandbox dying under it, which is what a
// supervisor that could not run the entrypoint does within milliseconds.
func (p *Provider) awaitStarted(ctx context.Context, id string, b bundle.Bundle) error {
	deadline := time.Now().Add(startGrace)

	for {
		started, err := hasStarted(b.ReadyFile)
		if err != nil {
			return err
		}
		if started {
			return nil
		}

		status, err := p.Status(ctx, id)
		if err != nil {
			return err
		}
		if !status.Alive() {
			return p.neverStarted(id, b)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the entrypoint of sandbox %s did not report that it started within %s", id, startGrace)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for the entrypoint of %s to start: %w", id, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// neverStarted names why the sandbox is already gone, quoting what the supervisor printed on its way
// out: the caller gives the state directory back, so the log dies with it.
func (p *Provider) neverStarted(id string, b bundle.Bundle) error {
	// The supervisor may have written the handshake between the read above and the status check.
	started, err := hasStarted(b.ReadyFile)
	if err != nil {
		return err
	}
	if started {
		return nil
	}
	refused, err := bundle.ReadNotStarted(id, b.ExitFile)
	if err != nil {
		return err
	}
	if refused != nil {
		return refused
	}

	path, err := p.LogPath(id)
	if err != nil {
		return err
	}

	return &models.EntrypointNotStartedError{Sandbox: id, Err: diagnostics(path)}
}

// hasStarted reports whether the supervisor wrote its handshake. The file arrives by rename, so its
// presence is the whole answer, and a link the guest put there, even a loop, is presence too (SHARD-630).
func hasStarted(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lstat %s: %w", path, err)
	}

	return true, nil
}

// diagnostics quotes the tail of the sandbox output as the cause of the error that reports it.
func diagnostics(path string) error {
	blob, err := readTail(path)
	if err != nil {
		return fmt.Errorf("its diagnostics were unreadable: %w", err)
	}

	text := strings.TrimSpace(string(blob))
	if text == "" {
		return errors.New("it printed nothing")
	}

	return errors.New(text)
}

// readTail keeps the last diagnosticTail bytes, because the guest writes to this file for as long
// as it lives.
func readTail(path string) (blob []byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	from := max(info.Size()-diagnosticTail, 0)

	return io.ReadAll(io.NewSectionReader(f, from, info.Size()-from))
}

// Stop is the only thing that ends a sandbox. It signals, waits out grace, then kills.
func (p *Provider) Stop(ctx context.Context, id string, grace time.Duration) error {
	status, err := p.Status(ctx, id)
	if err != nil {
		return err
	}

	// runsc refuses to signal a container whose entrypoint never started, so only safeDelete ends that one.
	if status.State == models.StateCreated {
		if err := p.safeDelete(ctx, id); err != nil {
			return err
		}

		return p.unmount(id, status.Exists)
	}

	// Nothing runs, so only the mount is left; runsc refuses to signal a paused sandbox whose sentry has exited (SHARD-336).
	if !status.Alive() {
		return p.unmount(id, status.Exists)
	}

	// A frozen sentry delivers no signal, and only a daemon that died between the freeze and the checkpoint leaves one behind.
	if status.State == models.StatePaused {
		if err := p.runsc.Resume(ctx, id); err != nil {
			if errors.Is(err, runsc.ErrUnreachable) {
				return p.endWedged(ctx, id, status.Exists)
			}

			return err
		}
	}

	// TERM goes to PID 1, which is shard-init: it forwards the signal to the entrypoint and then exits.
	if err := p.runsc.Kill(ctx, id, "TERM", false); err != nil && !gone(err) {
		if errors.Is(err, runsc.ErrUnreachable) {
			return p.endWedged(ctx, id, status.Exists)
		}

		return err
	}

	stopped, err := p.awaitStopped(ctx, id, grace)
	if err != nil {
		return err
	}

	if !stopped {
		if err := p.kill(ctx, id); err != nil {
			return err
		}
	}

	// runsc still holds a sandbox it has stopped, so the status read above is what owns the mount.
	return p.unmount(id, status.Exists)
}

// endWedged ends a cut-paused wedged sentry: safeDelete kills any pids its own cgroup still holds, takes an empty one as already gone, and drops the state (SHARD-411).
func (p *Provider) endWedged(ctx context.Context, id string, held bool) error {
	if err := p.safeDelete(ctx, id); err != nil {
		return err
	}

	return p.unmount(id, held)
}

func (p *Provider) kill(ctx context.Context, id string) error {
	if err := p.runsc.Kill(ctx, id, "KILL", true); err != nil && !gone(err) {
		return err
	}

	stopped, err := p.awaitStopped(ctx, id, killGrace)
	if err != nil {
		return err
	}
	if !stopped {
		return fmt.Errorf("sandbox %s is still running %s after SIGKILL", id, killGrace)
	}

	return nil
}

// Remove ends the sandbox and forgets runsc's own state. The record and the state directory belong to the repository.
func (p *Provider) Remove(ctx context.Context, id string) error {
	// First, so a restore cannot bring the sandbox up again after the teardown below.
	if err := p.killRestores(ctx, id); err != nil {
		return err
	}

	// safeDelete sweeps the sandbox's own processes, removes its cgroup, then drops runsc's state.
	if err := p.safeDelete(ctx, id); err != nil {
		return err
	}

	// The cgroup is gone, so no process of the sandbox holds the rootfs, whether or not the runtime knew it.
	b, err := p.open(id)
	if err != nil {
		return err
	}

	return b.Unmount()
}

// unmount drops the merged view. The upper layer stays, which is what a later create reads back.
// held says whether runsc knew the sandbox, because only that answers who owns the rootfs.
func (p *Provider) unmount(id string, held bool) error {
	b, err := p.open(id)
	if err != nil {
		return err
	}

	if err := orphaned(b, id, held); err != nil {
		return err
	}

	return b.Unmount()
}

// orphaned refuses a rootfs that stands while runsc holds nothing: something deleted the metadata by
// hand, and the sandbox that rootfs belongs to may still be running.
func orphaned(b bundle.Bundle, id string, held bool) error {
	if held {
		return nil
	}

	mounted, err := b.Mounted()
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}

	return fmt.Errorf("runsc does not hold sandbox %s but its rootfs is still mounted at %s", id, b.RootFS)
}

// gone reports whether a signal failed because the sandbox had already ended, which is what a stop wants.
func gone(err error) bool {
	return errors.Is(err, runsc.ErrNotRunning) || errors.Is(err, runsc.ErrNotFound)
}

// Exec runs a command in a sandbox that already runs. It is not the entrypoint: the supervisor never
// sees it, and Ctrl-C during one ends this command alone, because only Stop ends a sandbox.
func (p *Provider) Exec(ctx context.Context, id string, spec models.ExecSpec) (models.ExitStatus, error) {
	if len(spec.Argv) == 0 {
		return models.ExitStatus{}, fmt.Errorf("sandbox %s: exec has no command to run", id)
	}

	// runsc exec writes its own startup failures to the guest's stderr, so an exit code alone cannot
	// tell a broken exec from a command that failed. Refuse anything but a running sandbox first.
	status, err := p.Status(ctx, id)
	if err != nil {
		return models.ExitStatus{}, err
	}
	if !status.Exists {
		return models.ExitStatus{}, fmt.Errorf("sandbox %s does not exist on %s", id, Name)
	}
	if status.State != models.StateRunning {
		return models.ExitStatus{}, fmt.Errorf("sandbox %s is %s on %s, so nothing can run in it", id, status.State, Name)
	}

	b, err := p.open(id)
	if err != nil {
		return models.ExitStatus{}, err
	}

	opts, err := execOptions(b, spec)
	if err != nil {
		return models.ExitStatus{}, err
	}

	code, err := p.runsc.Exec(ctx, id, opts)
	if err != nil {
		return models.ExitStatus{}, execFailure(id, err)
	}

	// Signal stays 0: runsc reports an exec's exit code and nothing about the signal that ended it.
	return models.ExitStatus{Code: code}, nil
}

// Signal sends one signal to a running exec by the guest pid runsc reported for it.
func (p *Provider) Signal(ctx context.Context, id string, pid int, signal string) error {
	if err := p.runsc.Signal(ctx, id, pid, signal); err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}

	return nil
}

// StopApp signals shard-init, PID 1: USR1 terms the app and USR2 kills it, and both cancel every start again.
func (p *Provider) StopApp(ctx context.Context, id string, force bool) error {
	signal := "USR1"
	if force {
		signal = "USR2"
	}
	if err := p.runsc.Kill(ctx, id, signal, false); err != nil {
		return fmt.Errorf("sandbox %s: stop the app: %w", id, err)
	}

	return nil
}

// execFailure splits runsc's internal 128: a refused start gets a shell's own exit code, a lost wait the sentinel a pause can claim.
func execFailure(id string, err error) error {
	if lost, ok := errors.AsType[*runsc.ExecLostError](err); ok {
		return fmt.Errorf("sandbox %s: %w: %s", id, models.ErrExecLost, lost.Reason)
	}

	var start *runsc.ExecStartError
	if !errors.As(err, &start) {
		return err
	}

	code := models.CommandNotFoundExitCode
	if start.NotExecutable {
		code = models.CommandNotExecutableExitCode
	}

	return &models.CommandNotStartedError{Sandbox: id, Reason: start.Reason, Code: code}
}

// execOptions puts the exec where the entrypoint runs. config.json is the only record of that, and
// the rootfs it resolves a user against is the sandbox's live tree, not the image's.
func execOptions(b bundle.Bundle, spec models.ExecSpec) (runsc.ExecOptions, error) {
	runtime, err := b.Runtime()
	if err != nil {
		return runsc.ExecOptions{}, err
	}

	opts := runsc.ExecOptions{
		Bundle:  b.Dir,
		Argv:    spec.Argv,
		Env:     runspec.MergeEnv(runtime.Env, spec.Env),
		WorkDir: firstNonEmpty(spec.WorkDir, runtime.WorkDir, "/"),
		TTY:     spec.TTY,
		Stdin:   spec.Stdin,
		Stdout:  spec.Stdout,
		Stderr:  spec.Stderr,
		Report:  spec.Report,
	}

	// A named user is resolved against the sandbox's live tree; an unnamed one is the entrypoint's own,
	// which config.json records as the -user the supervisor was given.
	opts.User, opts.Groups = runtime.User, runtime.Groups
	if spec.User != "" {
		identity, err := bundle.ResolveUser(b.RootFS, spec.User)
		if err != nil {
			return runsc.ExecOptions{}, err
		}
		opts.User = fmt.Sprintf("%d:%d", identity.UID, identity.GID)
		opts.Groups = identity.Groups
	}

	return opts, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

// Wait blocks until the entrypoint exits. runsc wait cannot serve it: PID 1 is the supervisor and it
// never exits, so runsc wait would block forever. Watch the file shard-init writes instead.
func (p *Provider) Wait(ctx context.Context, id string) (models.ExitStatus, error) {
	b, err := p.open(id)
	if err != nil {
		return models.ExitStatus{}, err
	}

	for {
		exit, found, err := bundle.ReadExitStatus(b.ExitFile)
		if err != nil {
			return models.ExitStatus{}, err
		}
		if found {
			return exit, nil
		}

		status, err := p.Status(ctx, id)
		if err != nil {
			return models.ExitStatus{}, err
		}
		if !status.Alive() {
			// The supervisor may have written the file between the read above and this check.
			return lastExitStatus(b.ExitFile, id)
		}

		select {
		case <-ctx.Done():
			return models.ExitStatus{}, fmt.Errorf("wait for the entrypoint of %s: %w", id, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// ExitStatus reads how the entrypoint ended so far, nil while it still runs. It is a file read, so the
// liveness task polls it every tick, where Wait would block on an entrypoint that never exited.
func (p *Provider) ExitStatus(_ context.Context, id string) (*models.ExitStatus, error) {
	b, err := p.open(id)
	if err != nil {
		return nil, err
	}

	exit, found, err := bundle.ReadExitStatus(b.ExitFile)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	return &exit, nil
}

// Status asks the substrate, because a record saying running can outlive a shard restart.
func (p *Provider) Status(ctx context.Context, id string) (models.Status, error) {
	state, err := p.runsc.State(ctx, id)
	if errors.Is(err, runsc.ErrNotFound) {
		oom, err := p.oomKilled(id)
		if err != nil {
			return models.Status{}, err
		}

		return models.Status{OOMKilled: oom}, nil
	}
	if err != nil {
		return models.Status{}, err
	}

	// A daemon cut inside a live fork left its mark on the source, frozen or not, and a stale mark would pass a later real pause for a cut fork (SHARD-457).
	if got := stateOf(state.Status); got == models.StatePaused || got == models.StateRunning {
		thawed, err := p.thawCutFork(ctx, id)
		if err != nil {
			return models.Status{}, err
		}
		if thawed {
			return p.Status(ctx, id)
		}
	}

	status := models.Status{Exists: true, State: stateOf(state.Status), PID: state.PID}
	if status.Alive() {
		gone, err := p.stale(id, state)
		if err != nil {
			return models.Status{}, err
		}
		if gone {
			status.State, status.PID = models.StateStopped, 0
		}
	}
	status.Unstarted = status.State == models.StateCreated
	if !status.Alive() {
		status.OOMKilled, err = p.oomKilled(id)
		if err != nil {
			return models.Status{}, err
		}
	}

	return status, nil
}

// Restarts is a file read, not a substrate call, so a task may poll it every second.
func (p *Provider) Restarts(_ context.Context, id string) (models.RestartCount, error) {
	b, err := p.open(id)
	if err != nil {
		return models.RestartCount{}, err
	}

	return b.RestartCount()
}

// stale reports an alive runsc state whose pid is not this sandbox's live sentry (SHARD-411).
func (p *Provider) stale(id string, state runsc.State) (bool, error) {
	stat, err := os.ReadFile(filepath.Join(p.procRoot, strconv.Itoa(state.PID), "stat"))
	// PID 1 can reap the sentry after runsc's probe (SHARD-437), runsc never probes a paused one, and the sentry exits after a checkpoint (SHARD-336).
	if vanished(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the state of the sandbox process %d: %w", state.PID, err)
	}
	if zombieStat(string(stat)) {
		return true, nil
	}
	if state.Status != runsc.StatusPaused {
		return false, nil
	}

	// a clean pause ends the container in runsc, so a paused one with a live pid is a cut pause whose pid Linux reused unless it is still in this sandbox's cgroup.
	return p.foreignPid(state.PID, id)
}

// foreignPid reports a pid that is not in this sandbox's cgroup, so Linux reused it after the sentry exited.
func (p *Provider) foreignPid(pid int, id string) (bool, error) {
	pids, err := cgroup.Procs(cgroupDir(p.cgroupRoot, id))
	if errors.Is(err, cgroup.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("list the processes of sandbox %s: %w", id, err)
	}

	return !slices.Contains(pids, pid), nil
}

// vanished reads the two ways a process goes away under the read: /proc holds no such directory, or
// the kernel answers ESRCH because the process exited between the open and the read.
func vanished(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// zombieStat reads the state field, which follows the comm, and the comm may hold a parenthesis itself.
func zombieStat(stat string) bool {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 || i+2 >= len(stat) {
		return false
	}

	return stat[i+2] == 'Z'
}

// oomKilled asks the cgroup why a sandbox is gone. The OOM killer takes the sentry without running
// any of runsc's cleanup, so the cgroup and its counters outlive the sandbox and are the only record.
// A stop leaves the cgroup too, count and all, so a record that says stopped outranks this answer.
func (p *Provider) oomKilled(id string) (bool, error) {
	events, err := cgroup.MemoryEvents(cgroupDir(p.cgroupRoot, id))
	// A cgroup that is gone, or that has no memory controller, counted no OOM.
	if errors.Is(err, cgroup.ErrNotFound) || errors.Is(err, cgroup.ErrNoController) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read why sandbox %s ended: %w", id, err)
	}

	return events.OOM > 0, nil
}

// stateOf maps the five runsc statuses onto the four shard states. A container runsc is still
// creating has nothing in its guest running, which is what created means here.
func stateOf(status runsc.Status) models.State {
	switch status {
	case runsc.StatusRunning:
		return models.StateRunning
	case runsc.StatusPaused:
		return models.StatePaused
	case runsc.StatusStopped:
		return models.StateStopped
	default:
		return models.StateCreated
	}
}

// Pause checkpoints the sandbox into dir, then safeDelete sweeps its cgroup and forgets runsc's state, so the checkpoint plus the state directory is everything a resume needs.
func (p *Provider) Pause(ctx context.Context, id string, dir string) error {
	status, err := p.Status(ctx, id)
	if err != nil {
		return err
	}
	if !status.Exists {
		return fmt.Errorf("sandbox %s does not exist on %s", id, Name)
	}
	if status.State != models.StateRunning {
		return fmt.Errorf("sandbox %s is %s on %s: pause takes a running sandbox", id, status.State, Name)
	}

	b, err := p.open(id)
	if err != nil {
		return err
	}

	// The checkpoint is staged beside dir and swapped in whole, so dir never holds half of one.
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("clear the checkpoint directory %s: %w", tmp, err)
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fmt.Errorf("create the checkpoint directory %s: %w", tmp, err)
	}

	if err := p.runsc.Pause(ctx, id); err != nil {
		return err
	}

	// The checkpoint holds the memory alone: a resume restores over the bundle's own layer, and only a fork's capture copies one.
	if err := p.runsc.Checkpoint(ctx, id, tmp); err != nil {
		return p.lose(ctx, id, b, tmp, err)
	}

	// A filesystem without an atomic exchange refuses the install, after a checkpoint the sentry did not survive.
	if err := store.SwapDir(tmp, dir); err != nil {
		return p.lose(ctx, id, b, tmp, fmt.Errorf("install the checkpoint of sandbox %s: %w", id, err))
	}

	// ctx is the service's, cut from the client and bounded, so a Ctrl-C leaves no frozen sandbox and a wedged teardown holds no lock.
	return p.release(ctx, id, b, tmp)
}

// lose ends a pause that broke off after the checkpoint began: the sentry has exited, so nothing is left to thaw.
func (p *Provider) lose(ctx context.Context, id string, b bundle.Bundle, tmp string, err error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killGrace)
	defer cancel()

	return errors.Join(&models.LostError{Sandbox: id, Err: err}, p.release(ctx, id, b, tmp))
}

// release frees what runsc and the host still hold of a sandbox whose sentry has exited after a checkpoint.
func (p *Provider) release(ctx context.Context, id string, b bundle.Bundle, tmp string) error {
	// safeDelete sweeps the sandbox, removes its cgroup so a stale one does not unbound the resume, then drops the state.
	if err := p.safeDelete(ctx, id); err != nil {
		return err
	}

	// The layer stays, which is what the resume mounts again, and only the merged view goes; tmp holds the checkpoint this pause replaced.
	return errors.Join(os.RemoveAll(tmp), b.Unmount())
}

// Release frees what a cut pause left past its checkpoint, a frozen sentry or a mounted view, beside the checkpoint in dir (SHARD-366).
func (p *Provider) Release(ctx context.Context, id, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, checkpointFile)); err != nil {
		return fmt.Errorf("sandbox %s has no checkpoint in %s to release it beside: %w", id, dir, err)
	}

	// The same bound a lost pause's release has, over the probe too, so a wedged runsc stalls no boot and holds no lock.
	ctx, cancel := context.WithTimeout(ctx, killGrace)
	defer cancel()

	status, err := p.Status(ctx, id)
	if err != nil {
		return err
	}
	if status.Alive() && status.State != models.StatePaused {
		return fmt.Errorf("sandbox %s is %s on %s: only a frozen or ended sandbox is released beside its checkpoint", id, status.State, Name)
	}

	b, err := p.open(id)
	if err != nil {
		return err
	}

	return p.release(ctx, id, b, dir+".tmp")
}

// Resume brings the sandbox back from the checkpoint in dir over the writable layer the pause kept, as a new runsc container, the one the pause ended being gone for good.
func (p *Provider) Resume(ctx context.Context, id string, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, checkpointFile)); err != nil {
		return fmt.Errorf("sandbox %s has no checkpoint in %s: %w", id, dir, err)
	}

	stateDir, err := p.dirs(id)
	if err != nil {
		return err
	}

	b, err := bundle.Open(stateDir)
	if err != nil {
		return err
	}

	status, err := p.Status(ctx, id)
	if err != nil {
		return err
	}
	if status.Alive() {
		return fmt.Errorf("sandbox %s is %s on %s: resume takes a paused sandbox, which %s holds nothing of", id, status.State, Name, Name)
	}

	spec, err := p.reclaim(ctx, id, stateDir, b, status.Exists)
	if err != nil {
		return err
	}

	err = p.bringUp(ctx, spec, b.ExitFile, func(out, exit *os.File) error {
		return p.restore(ctx, id, runsc.RestoreOptions{Bundle: b.Dir, Image: dir, Stdout: out, Stderr: out, Stdin: exit})
	})
	if err != nil {
		return errors.Join(err, b.Unmount())
	}

	return nil
}

// AdoptStaging drops the checkpoint staging a cut pause left: resume reads the committed dir, never dir+".tmp", so a leftover stage is dead weight (SHARD-404).
func (p *Provider) AdoptStaging(dir string) error {
	return os.RemoveAll(dir + ".tmp")
}

// Fork freezes the running source, captures its memory and its layer, thaws the same sentry, and restores the fork from that capture over its own copy of the layer (SHARD-457).
func (p *Provider) Fork(ctx context.Context, sourceID string, spec models.SandboxSpec) error {
	source, err := p.Status(ctx, sourceID)
	if err != nil {
		return err
	}
	if source.State != models.StateRunning {
		return fmt.Errorf("sandbox %s is %s on %s: fork takes a running sandbox", sourceID, source.State, Name)
	}

	status, err := p.Status(ctx, spec.ID)
	if err != nil {
		return err
	}
	if status.Alive() {
		return fmt.Errorf("sandbox %s already exists on %s and is %s", spec.ID, Name, status.State)
	}

	existing, err := bundle.Open(spec.StateDir)
	if err != nil {
		return err
	}
	if err := orphaned(existing, spec.ID, status.Exists); err != nil {
		return err
	}

	capture := filepath.Join(spec.StateDir, captureDir)
	if err := p.capture(ctx, sourceID, capture); err != nil {
		return errors.Join(err, os.RemoveAll(capture))
	}

	// The fork's own disk, bounded the way the source's was, takes the layer copy.
	if err := existing.Provision(spec.Resources); err != nil {
		return err
	}

	b, err := p.bundles.Fork(capture, spec)
	if err != nil {
		return errors.Join(err, existing.Unmount())
	}

	rt, err := imageOf(b, spec.ID)
	if err != nil {
		return errors.Join(err, b.Unmount())
	}

	// A cgroup a removed sandbox of this id left behind would make the restore refuse the bound.
	if err := cgroup.Remove(cgroupDir(p.cgroupRoot, spec.ID)); err != nil {
		return errors.Join(fmt.Errorf("sweep the cgroup of sandbox %s: %w", spec.ID, err), b.Unmount())
	}

	if err := b.Mount(rt.RootFS); err != nil {
		return errors.Join(err, b.Unmount())
	}

	// The sentry's budget is in the memory image, so the fork is bound the way the source was.
	spec.Resources = rt.Resources

	err = p.bringUp(ctx, spec, b.ExitFile, func(out, exit *os.File) error {
		return p.restore(ctx, spec.ID, runsc.RestoreOptions{Bundle: b.Dir, Image: capture, Stdout: out, Stderr: out, Stdin: exit})
	})
	if err != nil {
		return errors.Join(err, b.Unmount())
	}

	return nil
}

// capture freezes the source, writes its memory and its layer into dir, and thaws it; the thaw is armed before the freeze and runs on every path, and a cut daemon's next read runs it from the mark.
func (p *Provider) capture(ctx context.Context, id, dir string) (err error) {
	src, err := p.open(id)
	if err != nil {
		return err
	}
	stateDir, err := p.dirs(id)
	if err != nil {
		return err
	}
	defer p.capturing.Delete(id)
	if err := p.markCapture(id, stateDir); err != nil {
		return err
	}
	thawed := false
	defer func() {
		if !thawed {
			err = errors.Join(err, p.thaw(ctx, id))
		}
	}()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create the capture directory %s: %w", dir, err)
	}
	frozen, cancel := context.WithTimeout(ctx, captureGrace)
	defer cancel()
	if err := p.runsc.Pause(frozen, id); err != nil {
		return err
	}
	if err := p.runsc.CheckpointRunning(frozen, id, dir); err != nil {
		return err
	}
	// The layer is copied while the guest is frozen, so the fork restores over the files its memory saw.
	if err := src.Export(frozen, dir); err != nil {
		return err
	}
	thawed = true

	return p.thaw(ctx, id)
}

// markCapture holds id for this process's capture and marks it frozen on disk, under forkMu, so a read's thaw never lands between the two.
func (p *Provider) markCapture(id, stateDir string) error {
	p.forkMu.Lock()
	defer p.forkMu.Unlock()
	p.capturing.Store(id, struct{}{})
	if err := os.WriteFile(filepath.Join(stateDir, forkFrozenFile), nil, 0o600); err != nil {
		return fmt.Errorf("mark sandbox %s frozen for its fork: %w", id, err)
	}

	return nil
}

// thaw resumes a source a live fork froze, past any cancel and within the kill grace, and drops its mark once runsc says it runs.
func (p *Provider) thaw(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killGrace)
	defer cancel()

	state, err := p.runsc.State(ctx, id)
	if err != nil {
		return fmt.Errorf("read sandbox %s before its thaw: %w", id, err)
	}
	// A cancel can cut the pause's answer after the freeze landed, or before it did, so only a frozen source is resumed.
	if stateOf(state.Status) == models.StatePaused {
		if err := p.runsc.Resume(ctx, id); err != nil {
			return fmt.Errorf("resume sandbox %s after its fork's capture: %w", id, err)
		}
		if state, err = p.runsc.State(ctx, id); err != nil {
			return fmt.Errorf("read sandbox %s after its thaw: %w", id, err)
		}
	}
	if got := stateOf(state.Status); got != models.StateRunning {
		return fmt.Errorf("sandbox %s is %s after its fork's thaw, want running", id, got)
	}
	stateDir, err := p.dirs(id)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(stateDir, forkFrozenFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear the fork mark of sandbox %s: %w", id, err)
	}

	return nil
}

// thawCutFork thaws a source whose live fork a daemon cut left marked, frozen or already resumed, and says whether it did; a source with no mark, or one this process captures now, is left as it is.
func (p *Provider) thawCutFork(ctx context.Context, id string) (bool, error) {
	stateDir, err := p.dirs(id)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(filepath.Join(stateDir, forkFrozenFile))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the fork mark of sandbox %s: %w", id, err)
	}

	p.forkMu.Lock()
	defer p.forkMu.Unlock()
	if _, held := p.capturing.Load(id); held {
		return false, nil
	}

	return true, p.thaw(ctx, id)
}

// Snapshot copies the layers a stop kept into dir, and refuses a source that could still write them.
func (p *Provider) Snapshot(ctx context.Context, sourceID, dir string) error {
	source, err := p.open(sourceID)
	if err != nil {
		return err
	}

	// A live source writes its layer under the copy, and its rootfs mount hides the layer's whiteouts.
	status, err := p.Status(ctx, sourceID)
	if err != nil {
		return err
	}
	if status.Alive() {
		return fmt.Errorf("sandbox %s is %s on %s: stop it first, a snapshot copies what a stop kept", sourceID, status.State, Name)
	}
	mounted, err := source.Mounted()
	if err != nil {
		return err
	}
	if mounted {
		return fmt.Errorf("sandbox %s is still mounted at %s: a copy of its layer would miss what the mount holds", sourceID, source.RootFS)
	}

	return source.Snapshot(ctx, dir)
}

// imageOf reads back the image a bundle stacks over, and refuses one that is gone before anything runs.
func imageOf(b bundle.Bundle, id string) (bundle.Runtime, error) {
	rt, err := b.Runtime()
	if err != nil {
		return bundle.Runtime{}, err
	}
	if rt.RootFS == "" {
		return bundle.Runtime{}, fmt.Errorf("sandbox %s records no image rootfs, so nothing says what its writable layer stacks over", id)
	}
	if err := bundle.CheckImage(rt.RootFS); err != nil {
		return bundle.Runtime{}, fmt.Errorf("sandbox %s: %w", id, err)
	}

	return rt, nil
}

// LogPath is where the guest's stdout and stderr land. SHARD-23 turns it into shard logs.
func (p *Provider) LogPath(id string) (string, error) {
	dir, err := p.dirs(id)
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, logFile), nil
}

// HeldLogs is the output log: the runtime holds it, so the daemon bounds it by copy and truncate.
func (p *Provider) HeldLogs(id string) ([]string, error) {
	path, err := p.LogPath(id)
	if err != nil {
		return nil, err
	}

	return []string{path}, nil
}

// Environment is the bundle: its config.json is the one record of what the entrypoint runs with.
func (p *Provider) Environment(id string) (models.Environment, error) {
	return bundle.Opener(p.dirs).Environment(id)
}

// open finds the bundle of a sandbox this process did not create.
func (p *Provider) open(id string) (bundle.Bundle, error) {
	dir, err := p.dirs(id)
	if err != nil {
		return bundle.Bundle{}, err
	}

	return bundle.Open(dir)
}

// awaitStopped reports whether the sandbox ended within the budget. It never fails on a missing one:
// a removed sandbox is stopped by any definition a caller cares about.
func (p *Provider) awaitStopped(ctx context.Context, id string, budget time.Duration) (bool, error) {
	deadline := time.Now().Add(budget)

	for {
		status, err := p.Status(ctx, id)
		if err != nil {
			return false, err
		}
		if !status.Alive() {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}

		select {
		case <-ctx.Done():
			return false, fmt.Errorf("wait for sandbox %s to stop: %w", id, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// openLog appends, so a second create over the same state directory adds to the sandbox's output.
func openLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return f, nil
}

// lastExitStatus answers a wait on a sandbox that has already ended, which only Stop can have done.
func lastExitStatus(path, id string) (models.ExitStatus, error) {
	status, found, err := bundle.ReadExitStatus(path)
	if err != nil {
		return models.ExitStatus{}, err
	}
	if !found {
		return models.ExitStatus{}, fmt.Errorf("sandbox %s: %w", id, models.ErrNoExitStatus)
	}

	return status, nil
}
