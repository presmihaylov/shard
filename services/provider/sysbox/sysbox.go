// Package sysbox runs sandboxes on Sysbox by driving bare sysbox-runc. Sysbox is the substrate that
// runs Docker and systemd inside the sandbox, and it has no snapshot at all: Capabilities is all
// false and the three optional verbs refuse by name.
package sysbox

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
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/pkg/sysboxrunc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/runspec"
)

// Name is the substrate, as the record and every refusal name it.
const Name = "sysbox"

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

// Provider implements models.Provider on Sysbox. The snapshot verbs are NoSnapshots' refusals.
type Provider struct {
	models.NoSnapshots

	runc    *sysboxrunc.Runner
	bundles *bundle.Service
	dirs    StateDirs
	// cgroupRoot is the host cgroup v2 mount. A test points it at a directory it can write.
	cgroupRoot string
}

func New(runner *sysboxrunc.Runner, bundles *bundle.Service, dirs StateDirs) (*Provider, error) {
	if runner == nil || bundles == nil || dirs == nil {
		return nil, errors.New("the sysbox provider needs a sysbox-runc runner, a bundle service and a state directory lookup")
	}

	return &Provider{NoSnapshots: models.NoSnapshots{Provider: Name}, runc: runner, bundles: bundles, dirs: dirs, cgroupRoot: cgroup.Root}, nil
}

func (p *Provider) Name() string { return Name }

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

// create runs sysbox-runc create over the log the container inherits. The memory bound rides in
// config.json and runc applies it to the cgroup itself, so nothing here touches the cgroup after.
func (p *Provider) create(ctx context.Context, spec models.SandboxSpec, b bundle.Bundle) (err error) {
	// A create over a state directory that already ran must not let the previous run answer a wait or
	// a start, so both of the supervisor's files go before anything else runs.
	for _, stale := range []string{b.ExitFile, b.ReadyFile} {
		if err := os.Remove(stale); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clear %s: %w", stale, err)
		}
	}

	out, err := openLog(filepath.Join(spec.StateDir, logFile))
	if err != nil {
		return err
	}
	// The container keeps its own copy of the fd, so closing ours does not cut the guest's output off.
	defer func() { err = errors.Join(err, out.Close()) }()

	return p.runc.Create(ctx, spec.ID, sysboxrunc.CreateOptions{Bundle: b.Dir, Stdout: out, Stderr: out})
}

// Start runs the entrypoint. runc never starts a stopped container again, so a stopped sandbox is
// re-created first over the writable layer its state directory kept. It returns only once the
// supervisor says the entrypoint forked, because runc start reads nothing back.
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

	if err := p.runc.Start(ctx, id); err != nil {
		return err
	}

	return p.awaitStarted(ctx, id, b)
}

// recreate is how a stopped sandbox runs again: the old container goes and a new one comes up over
// the same bundle, whose writable layer and config.json the stop kept.
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
		if err := p.runc.Delete(ctx, id, true); err != nil {
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

	// runc refuses to signal a container whose entrypoint never started, so only a delete ends that one.
	if status.State == models.StateCreated {
		if err := p.runc.Delete(ctx, id, true); err != nil {
			return err
		}

		return p.unmount(id, status.Exists)
	}

	// A frozen cgroup delivers no signal. Nothing of shard's freezes a Sysbox sandbox, so this is
	// only ever a container something else paused by hand.
	if status.State == models.StatePaused {
		if err := p.runc.Resume(ctx, id); err != nil {
			return err
		}
	}

	// TERM goes to PID 1, which is shard-init: it forwards the signal to the entrypoint and then exits.
	if err := p.runc.Kill(ctx, id, "TERM", false); err != nil && !gone(err) {
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

	// sysbox-runc still holds a sandbox it has stopped, so the status read above is what owns the mount.
	return p.unmount(id, status.Exists)
}

func (p *Provider) kill(ctx context.Context, id string) error {
	if err := p.runc.Kill(ctx, id, "KILL", true); err != nil && !gone(err) {
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
	// sysbox-runc delete --force exits 0 for an id it never held, so only a status read says who owns the rootfs.
	status, err := p.Status(ctx, id)
	if err != nil {
		return err
	}

	// --force, because a running sandbox holds the rootfs.
	if err := p.runc.Delete(ctx, id, true); err != nil {
		return err
	}

	// runc drops the cgroup of a sandbox it holds; a killed one leaves it, and its counters, behind.
	if err := cgroup.Remove(cgroupDir(p.cgroupRoot, id)); err != nil {
		return fmt.Errorf("sweep the cgroup of sandbox %s: %w", id, err)
	}

	// The repository removes the directory after this, and it must never remove a live mount.
	return p.unmount(id, status.Exists)
}

// unmount drops the merged view. The upper layer stays, which is what a later create reads back.
// held says whether sysbox-runc knew the sandbox, because only that answers who owns the rootfs.
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
	return errors.Is(err, sysboxrunc.ErrNotRunning) || errors.Is(err, sysboxrunc.ErrNotFound)
}

// Exec runs a command in a sandbox that already runs. It is not the entrypoint: the supervisor never
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

	b, err := p.open(id)
	if err != nil {
		return models.ExitStatus{}, err
	}

	opts, err := execOptions(b, spec)
	if err != nil {
		return models.ExitStatus{}, err
	}

	code, err := p.runc.Exec(ctx, id, opts)
	if err != nil {
		return models.ExitStatus{}, notStarted(id, err)
	}

	// Signal stays 0: runc reports an exec's exit code and nothing about the signal that ended it.
	return models.ExitStatus{Code: code}, nil
}

// notStarted gives a command the driver refused to start a name the cli can answer with a shell's
// own exit code. The driver looked the command up on the host, so the reason is the shell's wording.
func notStarted(id string, err error) error {
	var lookup *sysboxrunc.LookupError
	if !errors.As(err, &lookup) {
		return err
	}

	code := models.CommandNotFoundExitCode
	if lookup.NotExecutable {
		code = models.CommandNotExecutableExitCode
	}

	return &models.CommandNotStartedError{Sandbox: id, Reason: lookup.Reason, Code: code}
}

// execOptions puts the exec where the entrypoint runs. config.json is the only record of that, and
// the rootfs it resolves a user and the command against is the sandbox's live tree, not the image's.
func execOptions(b bundle.Bundle, spec models.ExecSpec) (sysboxrunc.ExecOptions, error) {
	runtime, err := b.Runtime()
	if err != nil {
		return sysboxrunc.ExecOptions{}, err
	}

	opts := sysboxrunc.ExecOptions{
		Argv:    spec.Argv,
		Env:     runspec.MergeEnv(runtime.Env, spec.Env),
		WorkDir: firstNonEmpty(spec.WorkDir, runtime.WorkDir, "/"),
		RootFS:  b.RootFS,
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
			return sysboxrunc.ExecOptions{}, err
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

// Wait blocks until the entrypoint exits. runc wait cannot serve it: PID 1 is the supervisor and it
// never exits, so it would block forever. Watch the file shard-init writes instead.
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

// Status asks the substrate, because a record saying running can outlive a shard restart. runc reads
// the init process itself and calls a reaped or zombie one stopped, so nothing here second-guesses it.
func (p *Provider) Status(ctx context.Context, id string) (models.Status, error) {
	state, err := p.runc.State(ctx, id)
	if errors.Is(err, sysboxrunc.ErrNotFound) {
		return models.Status{OOMKilled: p.oomKilled(id)}, nil
	}
	if err != nil {
		return models.Status{}, err
	}

	status := models.Status{Exists: true, State: stateOf(state.Status), PID: state.PID}
	if !status.Alive() {
		status.OOMKilled = p.oomKilled(id)
	}

	return status, nil
}

// oomKilled asks the cgroup why a sandbox is gone. The OOM killer takes a guest process without
// running any of runc's cleanup, so the cgroup and its counters outlive the sandbox and are the only
// record. A stop leaves the cgroup too, count and all, so a record that says stopped outranks this answer.
func (p *Provider) oomKilled(id string) bool {
	events, err := cgroup.MemoryEvents(cgroupDir(p.cgroupRoot, id))
	if err != nil {
		return false
	}

	return events.OOM > 0
}

// cgroupDir is the host side of the path the bundle names.
func cgroupDir(root, id string) string {
	return filepath.Join(root, bundle.CgroupsPath(id))
}

// stateOf maps the runc statuses onto the four shard states. A container runc is still creating has
// nothing in its guest running, which is what created means here.
func stateOf(status sysboxrunc.Status) models.State {
	switch status {
	case sysboxrunc.StatusRunning:
		return models.StateRunning
	case sysboxrunc.StatusPaused:
		return models.StatePaused
	case sysboxrunc.StatusStopped:
		return models.StateStopped
	default:
		return models.StateCreated
	}
}

// Clone is a start after a stop under a new id: the source's layers are copied and its entrypoint runs again.
func (p *Provider) Clone(ctx context.Context, sourceID string, spec models.SandboxSpec) error {
	source, err := p.open(sourceID)
	if err != nil {
		return err
	}

	// A live source writes its layer under the copy, and its rootfs mount hides the layer's whiteouts.
	sourceStatus, err := p.Status(ctx, sourceID)
	if err != nil {
		return err
	}
	if sourceStatus.Alive() {
		return fmt.Errorf("sandbox %s is %s on %s: stop it first, clone copies what a stop kept", sourceID, sourceStatus.State, Name)
	}
	mounted, err := source.Mounted()
	if err != nil {
		return err
	}
	if mounted {
		return fmt.Errorf("sandbox %s is still mounted at %s: a copy of its layer would miss what the mount holds", sourceID, source.RootFS)
	}
	if _, err := imageOf(source, sourceID); err != nil {
		return err
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

	b, err := p.bundles.Clone(source, spec)
	if err != nil {
		return err
	}

	rt, err := imageOf(b, spec.ID)
	if err != nil {
		return err
	}

	// A cgroup a removed sandbox of this id left behind would carry its counters into the clone.
	if err := cgroup.Remove(cgroupDir(p.cgroupRoot, spec.ID)); err != nil {
		return fmt.Errorf("sweep the cgroup of sandbox %s: %w", spec.ID, err)
	}

	if err := b.Mount(rt.RootFS); err != nil {
		return err
	}

	// config.json carries the source's bound, so the clone is bound the way the source was.
	spec.Resources = rt.Resources

	if err := p.create(ctx, spec, b); err != nil {
		return errors.Join(err, b.Unmount())
	}

	if err := p.runc.Start(ctx, spec.ID); err != nil {
		return err
	}

	return p.awaitStarted(ctx, spec.ID, b)
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

// LogPath is where the guest's stdout and stderr land.
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
