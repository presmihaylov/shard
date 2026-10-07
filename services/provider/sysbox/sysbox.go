// Package sysbox runs sandboxes on bare sysbox-runc, for Docker and systemd inside them; every checkpoint verb refuses by name.
package sysbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/pkg/launch"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/pkg/runc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/runspec"
)

// Name is the substrate, as the record and every refusal name it.
const Name = "sysbox"

// Binary is the runc fork the provider drives, on PATH on a Sysbox host.
const Binary = "sysbox-runc"

// Userns is the one mapping Sysbox CE gives every container, so the netns shard makes for a sandbox
// has to be owned by a user namespace with exactly it or the guest has no CAP_NET_ADMIN over it.
// One mapping for all sandboxes is why Sysbox CE is a single-tenant substrate.
var Userns = netns.IDMapping{HostID: 165536, Size: 65536}

// logFile holds the guest's stdout and stderr, interleaved the way a terminal would show them.
const logFile = "output.log"

const (
	// pollInterval paces every wait here. sysbox-runc state is a process spawn, so it is not free.
	pollInterval = 100 * time.Millisecond
	// killGrace bounds the wait after SIGKILL, which nothing in the container can refuse.
	killGrace = 10 * time.Second
	// startGrace bounds the wait for the supervisor's handshake, which it writes as soon as it forks.
	startGrace = 30 * time.Second
)

// diagnosticTail bounds what a failed start quotes back from the sandbox's own output.
const diagnosticTail = 4 << 10

// StateDirs answers where a sandbox's directory is. sandboxstate.Repository.Dir is what shard passes.
type StateDirs func(id string) (string, error)

var _ models.Provider = (*Provider)(nil)

// Provider implements models.Provider on Sysbox. The checkpoint verbs are NoCheckpoints' refusals.
type Provider struct {
	models.NoCheckpoints

	runner  *runc.Runner
	bundles *bundle.Service
	dirs    StateDirs
	// cgroupRoot is the host cgroup v2 mount. A test points it at a directory it can write.
	cgroupRoot string
	// procRoot is where a reopen finds PID 1's fd 0. A test points it at a directory it wrote.
	procRoot string
	exits    exitChannels
}

func New(runner *runc.Runner, bundles *bundle.Service, dirs StateDirs) (*Provider, error) {
	if runner == nil || bundles == nil || dirs == nil {
		return nil, errors.New("the sysbox provider needs a sysbox-runc runner, a bundle service and a state directory lookup")
	}

	return &Provider{NoCheckpoints: models.NoCheckpoints{Provider: Name}, runner: runner, bundles: bundles, dirs: dirs, cgroupRoot: cgroup.Root, procRoot: "/proc"}, nil
}

func (p *Provider) Name() string { return Name }

// CheckResources takes every bound: zero is unbounded on Linux, and a cgroup holds any size.
func (p *Provider) CheckResources(models.Resources) error { return nil }

// Userns is the mapping every sandbox's network namespace must belong to, so the guest owns it.
func (p *Provider) Userns() netns.IDMapping { return Userns }

// ReleaseRoot has nothing to give back: sysbox-runc pins no mount under its root between sandboxes.
func (p *Provider) ReleaseRoot() error { return nil }

// Create builds the bundle, stacks the writable layer over the image and prepares the container.
func (p *Provider) Create(ctx context.Context, spec models.SandboxSpec) error {
	// A live id must not be re-created: the rollback below would unmount the rootfs the first one runs on.
	status, err := p.Status(ctx, spec.ID)
	if err != nil {
		return err
	}
	if status.Alive() {
		return fmt.Errorf("sandbox %s already exists on %s and is %s", spec.ID, Name, status.State)
	}

	// A rootfs that stands while sysbox-runc holds nothing may still be a live sandbox's, so never build over it.
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

// create runs sysbox-runc create over the log the container inherits. runc applies the memory bound
// from config.json; boundMemory then sets the two OOM knobs runc leaves alone.
func (p *Provider) create(ctx context.Context, spec models.SandboxSpec, b bundle.Bundle) (err error) {
	// A fresh create must not inherit the old process table, readiness, or spec-change mark.
	if err := b.ClearRun(); err != nil {
		return err
	}

	out, err := openLog(filepath.Join(spec.StateDir, logFile))
	if err != nil {
		return err
	}
	// The container keeps its own copy of the fd, so closing ours does not cut the guest's output off.
	defer func() { err = errors.Join(err, out.Close()) }()

	if err := p.exits.drop(spec.ID); err != nil {
		return err
	}

	// shard-init writes its process table on its fd 0, a sealed page guest root can write but never grow (SHARD-419).
	exit, err := newExitChannel(b)
	if err != nil {
		return err
	}

	// runc makes the shard parent at 0755 only when it is missing, so one an older daemon made at 0750 is opened here.
	if err := cgroup.EnsureParent(filepath.Join(p.cgroupRoot, bundle.CgroupParent)); err != nil {
		return errors.Join(err, exit.Close())
	}

	if err := p.runner.Create(ctx, spec.ID, runc.CreateOptions{Bundle: b.Dir, Stdout: out, Stderr: out, Stdin: exit}); err != nil {
		return errors.Join(err, exit.Close())
	}

	if err := boundMemory(p.cgroupRoot, spec); err != nil {
		// runc made the container, so a failed bound must delete it, or it dangles on the rootfs the caller drops.
		return errors.Join(err, p.runner.Delete(ctx, spec.ID, true), exit.Close())
	}

	if err := boundPids(p.cgroupRoot, spec.ID); err != nil {
		return errors.Join(err, p.runner.Delete(ctx, spec.ID, true), exit.Close())
	}

	// The daemon keeps its own fd, so the last table outlives PID 1 until Remove or the next create.
	p.exits.put(spec.ID, exit)

	return nil
}

// boundPids caps the host cgroup on every launch, so a config.json written before pids were bounded is capped too.
func boundPids(root, id string) error {
	if err := cgroup.SetPidsMax(cgroupDir(root, id), bundle.PidsMax); err != nil {
		return fmt.Errorf("bound the pids of sandbox %s: %w", id, err)
	}

	return nil
}

// boundMemory sets the two knobs runc leaves out, so a memory bound kills the whole sandbox and its record says so.
func boundMemory(root string, spec models.SandboxSpec) error {
	if bundle.MemoryBound(spec.Resources) == 0 {
		return nil
	}

	dir := cgroupDir(root, spec.ID)

	// A cgroup that may swap reclaims to disk under pressure instead of dying at its ceiling.
	if err := cgroup.SetMemorySwapMax(dir, 0); err != nil {
		return fmt.Errorf("pin the swap of sandbox %s to none: %w", spec.ID, err)
	}

	// The OOM killer would take one guest process and leave the sandbox up, so group the whole kill.
	if err := cgroup.SetOOMGroup(dir); err != nil {
		return fmt.Errorf("group the OOM kill of sandbox %s: %w", spec.ID, err)
	}

	return nil
}

// Start re-creates a stopped sandbox over its kept layer, and returns once shard-init takes process requests, as runc start reads nothing back.
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

	if err := p.runner.Start(ctx, id); err != nil {
		return err
	}

	return p.awaitStarted(ctx, id, b)
}

// recreate gives a stopped or changed created sandbox a fresh runtime over its preserved bundle.
func (p *Provider) recreate(ctx context.Context, id, dir string, b bundle.Bundle, held bool) error {
	if err := orphaned(b, id, held); err != nil {
		return err
	}

	// Everything the new run needs is checked before the old container goes, so a refusal costs nothing.
	rt, err := imageOf(b, id)
	if err != nil {
		return err
	}

	if held {
		if err := p.runner.Delete(ctx, id, true); err != nil {
			return err
		}
	}

	// A cgroup a killed container left behind would carry its counters into the new run.
	if err := cgroup.Remove(cgroupDir(p.cgroupRoot, id)); err != nil {
		return fmt.Errorf("sweep the cgroup of sandbox %s: %w", id, err)
	}

	if err := b.Mount(rt.RootFS); err != nil {
		return err
	}

	spec := models.SandboxSpec{ID: id, StateDir: dir, Resources: rt.Resources}
	if err := p.create(ctx, spec, b); err != nil {
		return errors.Join(err, b.Unmount())
	}

	return nil
}

// awaitStarted watches for the handshake and for the sandbox dying under it, which is what a
// supervisor that could not set up does within milliseconds.
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
			return fmt.Errorf("shard-init in sandbox %s did not report that it started within %s", id, startGrace)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for shard-init in %s to start: %w", id, ctx.Err())
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

	path, err := p.outputLog(id)
	if err != nil {
		return err
	}

	return fmt.Errorf("sandbox %s: shard-init did not start: %w", id, diagnostics(path))
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

	// runc refuses to signal a container whose init never started, so only a delete ends that one.
	if status.State == models.StateCreated {
		if err := p.runner.Delete(ctx, id, true); err != nil {
			return err
		}

		return p.unmount(id, status.Exists)
	}

	// A frozen cgroup delivers no signal. Nothing of shard's freezes a Sysbox sandbox, so this is
	// only ever a container something else paused by hand.
	if status.State == models.StatePaused {
		if err := p.runner.Resume(ctx, id); err != nil {
			return err
		}
	}

	b, err := p.open(id)
	if err != nil {
		return err
	}
	// Hold the page before PID 1 exits: a daemon that restarted since create finds it only through a live PID 1.
	err = p.collect(ctx, id, b)
	// A guest that replaced fd 0 has no table left to keep, and must not keep its sandbox from stopping.
	if err != nil && !errors.Is(err, models.ErrExitChannelReplaced) {
		return err
	}

	// TERM goes to PID 1, which is shard-init: it terms every process, reaps them and then exits.
	if err := p.runner.Kill(ctx, id, "TERM", false); err != nil && !gone(err) {
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

	// Copy the table before Stop returns: with PID 1 gone, this daemon holds the last fd of the page.
	if err := p.collect(ctx, id, b); err != nil {
		return err
	}

	// sysbox-runc still holds a sandbox it has stopped, so the status read above is what owns the mount.
	if err := orphaned(b, id, status.Exists); err != nil {
		return err
	}

	// The disk stays up: sysbox-mgr chowns the upper layer back when the container is deleted, at the next start or at remove.
	return b.UnmountOverlay()
}

func (p *Provider) kill(ctx context.Context, id string) error {
	if err := p.runner.Kill(ctx, id, "KILL", true); err != nil && !gone(err) {
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

// Remove deletes sysbox-runc's own state. The record and the state directory belong to the repository.
func (p *Provider) Remove(ctx context.Context, id string) error {
	// --force, because a running sandbox holds the rootfs.
	if err := p.runner.Delete(ctx, id, true); err != nil {
		return err
	}

	// runc drops the cgroup of a sandbox it holds; a killed one leaves it, and its counters, behind.
	if err := cgroup.Remove(cgroupDir(p.cgroupRoot, id)); err != nil {
		return fmt.Errorf("sweep the cgroup of sandbox %s: %w", id, err)
	}

	if err := p.exits.drop(id); err != nil {
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
// held says whether sysbox-runc knew the sandbox, because only that answers who owns the rootfs.
func (p *Provider) unmount(id string, held bool) error {
	b, err := p.openHeld(id, held)
	if err != nil {
		return err
	}

	return b.Unmount()
}

// openHeld is the bundle of a sandbox whose rootfs may be dropped: sysbox-runc held it, or nothing stands on it.
func (p *Provider) openHeld(id string, held bool) (bundle.Bundle, error) {
	b, err := p.open(id)
	if err != nil {
		return bundle.Bundle{}, err
	}

	if err := orphaned(b, id, held); err != nil {
		return bundle.Bundle{}, err
	}

	return b, nil
}

// orphaned refuses a rootfs that stands while sysbox-runc holds nothing: something deleted the
// metadata by hand, and the sandbox that rootfs belongs to may still be running.
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

	return fmt.Errorf("sysbox-runc does not hold sandbox %s but its rootfs is still mounted at %s", id, b.RootFS)
}

// gone reports whether a signal failed because the sandbox had already ended, which is what a stop wants.
func gone(err error) bool {
	return errors.Is(err, runc.ErrNotRunning) || errors.Is(err, runc.ErrNotFound)
}

// Exec runs a command in a sandbox that already runs. It is no named process: the supervisor never
// sees it, and Ctrl-C during one ends this command alone, because only Stop ends a sandbox.
func (p *Provider) Exec(ctx context.Context, id string, spec models.ExecSpec) (models.ExitStatus, error) {
	if len(spec.Argv) == 0 {
		return models.ExitStatus{}, fmt.Errorf("sandbox %s: exec has no command to run", id)
	}

	// sysbox-runc exec reports its own startup failures as the command's exit 1, so an exit code
	// alone cannot tell a broken exec from a command that failed. Refuse anything but a running sandbox first.
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

	pid, err := p.confirmInit(id, status)
	if err != nil {
		return models.ExitStatus{}, err
	}
	if pid == 0 {
		return models.ExitStatus{}, fmt.Errorf("sandbox %s has no PID 1 on %s, so nothing can run in it", id, Name)
	}

	b, err := p.open(id)
	if err != nil {
		return models.ExitStatus{}, err
	}

	// sysbox-runc mounts its own overlay as the guest root, and the host's mount of the same layers can show a stale tree (SHARD-653).
	guest := filepath.Join(p.procRoot, strconv.Itoa(pid), "root")
	if err := bundle.CheckUserDatabases(guest); err != nil {
		return models.ExitStatus{}, err
	}

	opts, err := execOptions(b, guest, spec)
	if err != nil {
		return models.ExitStatus{}, err
	}

	code, err := p.runner.Exec(ctx, id, opts)
	if err != nil {
		return models.ExitStatus{}, notStarted(id, opts.WorkDir, err)
	}

	// Signal stays 0: runc reports an exec's exit code and nothing about the signal that ended it.
	return models.ExitStatus{Code: code}, nil
}

// Signal passes back the handle the driver reported for this sandbox's exec.
func (p *Provider) Signal(ctx context.Context, id string, pid int, signal string) error {
	if err := p.runner.Signal(ctx, id, pid, signal); err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}

	return nil
}

// notStarted gives a command whose execve never took a name the cli answers with a shell's own exit code.
func notStarted(id, workDir string, err error) error {
	var failed *launch.NotStartedError
	if !errors.As(err, &failed) {
		return err
	}
	// docker exec answers 126 for a work directory it cannot enter, whatever the errno.
	if failed.Chdir {
		return &models.CommandNotStartedError{Sandbox: id, Reason: launch.WorkDirReason(workDir, failed.Errno), Code: models.CommandNotExecutableExitCode}
	}

	code := models.CommandNotExecutableExitCode
	if failed.NotFound() {
		code = models.CommandNotFoundExitCode
	}

	return &models.CommandNotStartedError{Sandbox: id, Reason: failed.Reason(), Code: code}
}

// execOptions resolves users against the live sandbox because it can differ from the image, and reads HOME from guest, the root PID 1 sees.
func execOptions(b bundle.Bundle, guest string, spec models.ExecSpec) (runc.ExecOptions, error) {
	runtime, err := b.Runtime()
	if err != nil {
		return runc.ExecOptions{}, err
	}

	opts := runc.ExecOptions{
		Bundle:  b.Dir,
		Argv:    spec.Argv,
		Env:     runspec.ExecEnv(runtime.Env, spec.Env, spec.TTY),
		WorkDir: firstNonEmpty(spec.WorkDir, runtime.WorkDir, "/"),
		Launch:  bundle.GuestInitPath,
		TTY:     spec.TTY,
		Stdin:   spec.Stdin,
		Stdout:  spec.Stdout,
		Stderr:  spec.Stderr,
		Report:  spec.Report,
	}

	// A named user resolves against the live tree; an unnamed one is the sandbox's own, from a config.json annotation.
	opts.User, opts.Groups = runtime.User, runtime.Groups
	if spec.User != "" {
		identity, err := bundle.ResolveUser(b.RootFS, spec.User)
		if err != nil {
			return runc.ExecOptions{}, err
		}
		opts.User = fmt.Sprintf("%d:%d", identity.UID, identity.GID)
		opts.Groups = identity.Groups
	}

	opts.Env, err = bundle.AddHome(guest, opts.User, opts.Env)
	if err != nil {
		return runc.ExecOptions{}, err
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

// Status asks the substrate, because a record saying running can outlive a shard restart. runc reads
// the init process itself and calls a reaped or zombie one stopped, so nothing here second-guesses it.
func (p *Provider) Status(ctx context.Context, id string) (models.Status, error) {
	state, err := p.runner.State(ctx, id)
	if errors.Is(err, runc.ErrNotFound) {
		oom, err := p.oomKilled(id)
		if err != nil {
			return models.Status{}, err
		}

		return models.Status{OOMKilled: oom}, nil
	}
	if err != nil {
		return models.Status{}, err
	}

	status := models.Status{Exists: true, State: stateOf(state.Status), PID: state.PID}
	status.Unstarted = status.State == models.StateCreated
	if !status.Alive() {
		status.OOMKilled, err = p.oomKilled(id)
		if err != nil {
			return models.Status{}, err
		}
	}

	return status, nil
}

// oomKilled asks the cgroup why a sandbox is gone. The OOM killer takes a guest process without
// running any of runc's cleanup, so the cgroup and its counters outlive the sandbox and are the only
// record. A stop leaves the cgroup too, count and all, so a record that says stopped outranks this answer.
func (p *Provider) oomKilled(id string) (bool, error) {
	// The local count alone: a nested container that hits its own bound in the guest is not the sandbox's OOM (SHARD-364).
	events, err := cgroup.LocalMemoryEvents(cgroupDir(p.cgroupRoot, id))
	// A cgroup that is gone, or that has no memory controller, counted no OOM.
	if errors.Is(err, cgroup.ErrNotFound) || errors.Is(err, cgroup.ErrNoController) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read why sandbox %s ended: %w", id, err)
	}

	return events.OOM > 0, nil
}

// cgroupDir is the host side of the path the bundle names.
func cgroupDir(root, id string) string {
	return filepath.Join(root, bundle.CgroupsPath(id))
}

// stateOf maps the runc statuses onto the four shard states. A container runc is still creating has
// nothing in its guest running, which is what created means here.
func stateOf(status runc.Status) models.State {
	switch status {
	case runc.StatusRunning:
		return models.StateRunning
	case runc.StatusPaused:
		return models.StatePaused
	case runc.StatusStopped:
		return models.StateStopped
	default:
		return models.StateCreated
	}
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

// outputLog holds what shard-init and the runtime print, apart from each process's own log.
func (p *Provider) outputLog(id string) (string, error) {
	dir, err := p.dirs(id)
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, logFile), nil
}

// HeldLogs is the output log and each process log: the runtime and shard-init hold them, so the daemon bounds them by copy and truncate.
func (p *Provider) HeldLogs(id string) ([]string, error) {
	dir, err := p.dirs(id)
	if err != nil {
		return nil, err
	}
	b, err := bundle.Open(dir)
	if err != nil {
		return nil, err
	}
	logs, err := b.ProcessLogs()
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", id, err)
	}

	return append([]string{filepath.Join(dir, logFile)}, logs...), nil
}

// Environment is the bundle: its config.json is the one record of what every process runs with.
func (p *Provider) Environment(id string) (models.Environment, error) {
	b, err := p.open(id)
	if err != nil {
		return nil, err
	}
	// sysbox-runc shifts the upper layer by the one mapping Sysbox CE gives, whether config.json names it or not.
	b.Userns = Userns

	return b, nil
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
