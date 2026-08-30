// Package gvisor runs sandboxes on gVisor by driving bare runsc.
package gvisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/runspec"
)

const name = "gvisor"

// logFile holds the guest's stdout and stderr, interleaved the way a terminal would show them.
const logFile = "output.log"

const (
	// pollInterval paces every wait here. runsc state is a socket round trip, so it is not free.
	pollInterval = 100 * time.Millisecond
	// killGrace bounds the wait after SIGKILL, which the sentry cannot refuse.
	killGrace = 10 * time.Second
	// startGrace bounds the wait for the supervisor's handshake, which it writes as soon as it forks.
	startGrace = 30 * time.Second
)

// diagnosticTail bounds what a failed start quotes back from the sandbox's own output.
const diagnosticTail = 4 << 10

// checkpointFile is the one file every runsc checkpoint writes, so its absence says there is no snapshot.
const checkpointFile = "checkpoint.img"

// StateDirs answers where a sandbox's directory is. sandboxstate.Repository.Dir is what shard passes:
// every verb below takes an id, and shard runs no daemon that could remember the path from Create.
type StateDirs func(id string) (string, error)

var _ models.Provider = (*Provider)(nil)

// Provider implements models.Provider on gVisor.
type Provider struct {
	runsc   *runsc.Runner
	bundles *bundle.Service
	dirs    StateDirs
	caps    models.Capabilities
	// cgroupRoot is the host cgroup v2 mount. A test points it at a directory it can write.
	cgroupRoot string
}

func New(runner *runsc.Runner, bundles *bundle.Service, dirs StateDirs) (*Provider, error) {
	if runner == nil || bundles == nil || dirs == nil {
		return nil, errors.New("the gvisor provider needs a runsc runner, a bundle service and a state directory lookup")
	}

	// Capabilities is fixed once here, so it needs no context and cannot fail.
	caps := models.Capabilities{Pause: true, Resume: true, Fork: true}

	return &Provider{runsc: runner, bundles: bundles, dirs: dirs, caps: caps, cgroupRoot: cgroup.Root}, nil
}

func (p *Provider) Name() string { return name }

func (p *Provider) Capabilities() models.Capabilities { return p.caps }

// Create builds the bundle, stacks the writable layer over the image and prepares the container.
func (p *Provider) Create(ctx context.Context, spec models.SandboxSpec) error {
	// The sentry boots inside the cgroup runsc builds from this number, so a bound under its own cost
	// kills the create with nothing shard can read back.
	if mib := spec.Resources.MemoryMiB; mib > 0 && mib < MinimumMemoryMiB {
		return fmt.Errorf("sandbox %s asks for %d MiB, and %s needs at least %d MiB: the sentry itself costs about 30 MiB",
			spec.ID, mib, name, MinimumMemoryMiB)
	}

	// A live id must not be re-created: the rollback below would unmount the rootfs the first one runs on.
	status, err := p.Status(ctx, spec.ID)
	if err != nil {
		return err
	}
	if status.Alive() {
		return fmt.Errorf("sandbox %s already exists on %s and is %s", spec.ID, name, status.State)
	}

	// A rootfs that stands while runsc holds nothing may still be a live sandbox's, so never build over it.
	existing, err := bundle.Open(spec.StateDir)
	if err != nil {
		return err
	}
	if err := orphaned(existing, spec.ID, status.Exists); err != nil {
		return err
	}

	b, err := p.bundles.Build(spec)
	if err != nil {
		return err
	}

	if err := b.Mount(spec.RootFS); err != nil {
		return err
	}

	if err := p.create(ctx, spec, b); err != nil {
		// A half-created sandbox must not leave a mount behind, because nothing else knows to drop it.
		return errors.Join(err, b.Unmount())
	}

	return nil
}

func (p *Provider) create(ctx context.Context, spec models.SandboxSpec, b bundle.Bundle) error {
	// A create over a state directory that already ran must not let the previous run answer a wait or
	// a start, so both of the supervisor's files go before anything else runs.
	for _, stale := range []string{b.ExitFile, b.ReadyFile} {
		if err := os.Remove(stale); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clear %s: %w", stale, err)
		}
	}

	return p.bringUp(ctx, spec, func(out *os.File) error {
		return p.runsc.Create(ctx, spec.ID, runsc.CreateOptions{Bundle: b.Dir, Stdout: out, Stderr: out})
	})
}

// bringUp runs the runsc verb that forks the sandbox process, over the log it inherits and inside
// the cgroup shard owns, and then moves the host bounds where create and restore both need them.
func (p *Provider) bringUp(ctx context.Context, spec models.SandboxSpec, up func(out *os.File) error) (err error) {
	out, err := openLog(filepath.Join(spec.StateDir, logFile))
	if err != nil {
		return err
	}
	// The sandbox keeps its own copy of the fd, so closing ours does not cut the guest's output off.
	defer func() { err = errors.Join(err, out.Close()) }()

	// runsc rmdirs every cgroup it made on delete, the parent included, so the parent must be shard's.
	if err := cgroup.Ensure(filepath.Join(p.cgroupRoot, bundle.CgroupParent)); err != nil {
		return err
	}

	if err := up(out); err != nil {
		return err
	}

	if err := boundMemory(p.cgroupRoot, spec); err != nil {
		// The caller drops the rootfs mount, so a sandbox left created would run on a mount that is gone.
		return errors.Join(err, p.runsc.Delete(ctx, spec.ID, true))
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

	if !status.Alive() {
		if err := p.recreate(ctx, id, dir, b, status.Exists); err != nil {
			return err
		}
	}

	if err := p.runsc.Start(ctx, id); err != nil {
		return err
	}

	return p.awaitStarted(ctx, id, b)
}

// recreate is how a stopped sandbox runs again: runsc never starts one, so the container goes and a
// new one comes up over the same bundle, whose writable layer and config.json the stop kept.
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
		if err := p.runsc.Delete(ctx, id, true); err != nil {
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

	path, err := p.LogPath(id)
	if err != nil {
		return err
	}

	return fmt.Errorf("the entrypoint of sandbox %s did not start%s", id, diagnostics(path))
}

// hasStarted reports whether the supervisor wrote its handshake. The file arrives by rename, so its
// presence is the whole answer.
func hasStarted(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}

	return true, nil
}

// diagnostics quotes the tail of the sandbox output, as the suffix of the error that reports it.
func diagnostics(path string) string {
	blob, err := readTail(path)
	if err != nil {
		return fmt.Sprintf(": its diagnostics were unreadable: %v", err)
	}

	text := strings.TrimSpace(string(blob))
	if text == "" {
		return ": it printed nothing"
	}

	return ": " + text
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

	// runsc refuses to signal a container whose entrypoint never started, so only a delete ends that one.
	if status.State == models.StateCreated {
		if err := p.runsc.Delete(ctx, id, true); err != nil {
			return err
		}

		return p.unmount(id, status.Exists)
	}

	// A frozen sentry delivers no signal, and only a pause that broke off leaves one behind.
	if status.State == models.StatePaused {
		if err := p.runsc.Resume(ctx, id); err != nil {
			return err
		}
	}

	// TERM goes to PID 1, which is shard-init: it forwards the signal to the entrypoint and then exits.
	if err := p.runsc.Kill(ctx, id, "TERM", false); err != nil && !gone(err) {
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

// Remove deletes runsc's own state. The record and the state directory belong to the repository.
func (p *Provider) Remove(ctx context.Context, id string) error {
	// runsc delete --force exits 0 for an id it never held, so only a status read says who owns the rootfs.
	status, err := p.Status(ctx, id)
	if err != nil {
		return err
	}

	// --force, because a running sandbox holds the rootfs.
	if err := p.runsc.Delete(ctx, id, true); err != nil {
		return err
	}

	// runsc drops the cgroup of a sandbox it holds, and a stale one would unbound the next create of the id.
	if err := cgroup.Remove(cgroupDir(p.cgroupRoot, id)); err != nil {
		return fmt.Errorf("sweep the cgroup of sandbox %s: %w", id, err)
	}

	// The repository removes the directory after this, and it must never remove a live mount.
	return p.unmount(id, status.Exists)
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
		return models.ExitStatus{}, fmt.Errorf("sandbox %s does not exist on %s", id, name)
	}
	if status.State != models.StateRunning {
		return models.ExitStatus{}, fmt.Errorf("sandbox %s is %s on %s, so nothing can run in it", id, status.State, name)
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
		return models.ExitStatus{}, notStarted(id, err)
	}

	// Signal stays 0: runsc reports an exec's exit code and nothing about the signal that ended it.
	return models.ExitStatus{Code: code}, nil
}

// notStarted gives a command runsc refused to start a name the cli can answer with a shell's own
// exit code, because runsc reports every one of them as its internal 128.
func notStarted(id string, err error) error {
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
		Argv:    spec.Argv,
		Env:     runspec.MergeEnv(runtime.Env, spec.Env),
		WorkDir: firstNonEmpty(spec.WorkDir, runtime.WorkDir, "/"),
		TTY:     spec.TTY,
		Stdin:   spec.Stdin,
		Stdout:  spec.Stdout,
		Stderr:  spec.Stderr,
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
		exit, found, err := readExitStatus(b.ExitFile)
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

// Status asks the substrate, because a record saying running can outlive a shard restart.
func (p *Provider) Status(ctx context.Context, id string) (models.Status, error) {
	state, err := p.runsc.State(ctx, id)
	if errors.Is(err, runsc.ErrNotFound) {
		return models.Status{OOMKilled: p.oomKilled(id)}, nil
	}
	if err != nil {
		return models.Status{}, err
	}

	status := models.Status{Exists: true, State: stateOf(state.Status), PID: state.PID}
	if status.Alive() {
		dead, err := zombie(state.PID)
		if err != nil {
			return models.Status{}, err
		}
		if dead {
			status.State, status.PID = models.StateStopped, 0
		}
	}
	if !status.Alive() {
		status.OOMKilled = p.oomKilled(id)
	}

	return status, nil
}

// zombie reports a sandbox process that exited and waits for its reaper. runsc probes it with
// kill(pid, 0), which a zombie still answers, so runsc calls the sandbox running until PID 1 reaps it.
func zombie(pid int) (bool, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the state of the sandbox process %d: %w", pid, err)
	}

	return zombieStat(string(stat)), nil
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
func (p *Provider) oomKilled(id string) bool {
	events, err := cgroup.MemoryEvents(cgroupDir(p.cgroupRoot, id))
	if err != nil {
		return false
	}

	return events.OOM > 0
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

// Pause writes the sandbox into dir and then deletes it from runsc, because a checkpointed container
// still holds its whole memory until it is deleted. runsc then holds nothing, as after a stop before
// the entrypoint ran, and the snapshot plus the state directory is everything a resume needs.
func (p *Provider) Pause(ctx context.Context, id string, dir string) error {
	status, err := p.Status(ctx, id)
	if err != nil {
		return err
	}
	if !status.Exists {
		return fmt.Errorf("sandbox %s does not exist on %s", id, name)
	}
	if status.State != models.StateRunning {
		return fmt.Errorf("sandbox %s is %s on %s: pause takes a running sandbox", id, status.State, name)
	}

	b, err := p.open(id)
	if err != nil {
		return err
	}

	// The old snapshot stays until the new one is complete, so a failed pause loses nothing a fork needs.
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("clear the snapshot directory %s: %w", tmp, err)
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fmt.Errorf("create the snapshot directory %s: %w", tmp, err)
	}

	if err := p.runsc.Pause(ctx, id); err != nil {
		return err
	}

	// The layer is copied while the guest is frozen, so a fork restores over the files the memory saw.
	if err := errors.Join(p.runsc.Checkpoint(ctx, id, tmp), b.Export(tmp)); err != nil {
		// Only stop ends a sandbox, so one whose snapshot failed goes on running, even after a Ctrl-C.
		thaw, cancel := context.WithTimeout(context.WithoutCancel(ctx), killGrace)
		defer cancel()

		return errors.Join(err, p.runsc.Resume(thaw, id), os.RemoveAll(tmp))
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clear the snapshot directory %s: %w", dir, err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("move the snapshot into place: %w", err)
	}

	// The snapshot is complete, so a Ctrl-C from here on must not leave a frozen sandbox behind.
	ctx = context.WithoutCancel(ctx)

	if err := p.runsc.Delete(ctx, id, true); err != nil {
		return err
	}

	// runsc drops the cgroup of a sandbox it holds, and a stale one would unbound the resume.
	if err := cgroup.Remove(cgroupDir(p.cgroupRoot, id)); err != nil {
		return fmt.Errorf("sweep the cgroup of sandbox %s: %w", id, err)
	}

	// The layer stays, which is what the resume mounts again, and only the merged view goes.
	return b.Unmount()
}

// Resume brings the sandbox back from the snapshot in dir, over the writable layer the pause kept,
// as a new runsc container: the one the pause deleted is gone for good.
func (p *Provider) Resume(ctx context.Context, id string, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, checkpointFile)); err != nil {
		return fmt.Errorf("sandbox %s has no snapshot in %s: %w", id, dir, err)
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
		return fmt.Errorf("sandbox %s is %s on %s: resume takes a paused sandbox, which %s holds nothing of", id, status.State, name, name)
	}

	spec, err := p.reclaim(ctx, id, stateDir, b, status.Exists)
	if err != nil {
		return err
	}

	err = p.bringUp(ctx, spec, func(out *os.File) error {
		return p.runsc.Restore(ctx, id, runsc.RestoreOptions{Bundle: b.Dir, Image: dir, Stdout: out, Stderr: out})
	})
	if err != nil {
		return errors.Join(err, b.Unmount())
	}

	return nil
}

// Fork restores the snapshot in dir as a new sandbox over its own copy of the layer: two forks share nothing.
func (p *Provider) Fork(ctx context.Context, dir string, spec models.SandboxSpec) error {
	if _, err := os.Stat(filepath.Join(dir, checkpointFile)); err != nil {
		return fmt.Errorf("no snapshot to fork in %s: %w", dir, err)
	}

	status, err := p.Status(ctx, spec.ID)
	if err != nil {
		return err
	}
	if status.Alive() {
		return fmt.Errorf("sandbox %s already exists on %s and is %s", spec.ID, name, status.State)
	}

	existing, err := bundle.Open(spec.StateDir)
	if err != nil {
		return err
	}
	if err := orphaned(existing, spec.ID, status.Exists); err != nil {
		return err
	}

	b, err := p.bundles.Clone(dir, spec)
	if err != nil {
		return err
	}

	rt, err := imageOf(b, spec.ID)
	if err != nil {
		return err
	}

	// A cgroup a removed sandbox of this id left behind would make the restore refuse the bound.
	if err := cgroup.Remove(cgroupDir(p.cgroupRoot, spec.ID)); err != nil {
		return fmt.Errorf("sweep the cgroup of sandbox %s: %w", spec.ID, err)
	}

	if err := b.Mount(rt.RootFS); err != nil {
		return err
	}

	// The sentry's budget is in the memory image, so the fork is bound the way the source was.
	spec.Resources = rt.Resources

	err = p.bringUp(ctx, spec, func(out *os.File) error {
		return p.runsc.Restore(ctx, spec.ID, runsc.RestoreOptions{Bundle: b.Dir, Image: dir, Stdout: out, Stderr: out})
	})
	if err != nil {
		return errors.Join(err, b.Unmount())
	}

	return nil
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
	if _, err := os.Stat(rt.RootFS); err != nil {
		return bundle.Runtime{}, fmt.Errorf("sandbox %s stacks over an image rootfs that is gone: %w", id, err)
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

// readExitStatus reads what shard-init wrote. The file arrives by rename, so it never reads half of one.
func readExitStatus(path string) (models.ExitStatus, bool, error) {
	blob, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return models.ExitStatus{}, false, nil
	}
	if err != nil {
		return models.ExitStatus{}, false, fmt.Errorf("read %s: %w", path, err)
	}

	var status models.ExitStatus
	if err := json.Unmarshal(blob, &status); err != nil {
		return models.ExitStatus{}, false, fmt.Errorf("decode the exit status in %s: %w", path, err)
	}

	return status, true, nil
}

// lastExitStatus answers a wait on a sandbox that has already ended, which only Stop can have done.
func lastExitStatus(path, id string) (models.ExitStatus, error) {
	status, found, err := readExitStatus(path)
	if err != nil {
		return models.ExitStatus{}, err
	}
	if !found {
		return models.ExitStatus{}, fmt.Errorf("sandbox %s: %w", id, models.ErrNoExitStatus)
	}

	return status, nil
}
