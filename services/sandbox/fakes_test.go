package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/portforward"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/secret"
)

// recorder logs what the fakes were asked in order; a name in fail fails every call, a name#N the Nth only.
// The exec tests drive the service concurrently, so mu guards every access to calls and live.
type recorder struct {
	mu   sync.Mutex
	fail []string
	// cause is what a forced failure wraps, so a test can fail a call with a typed substrate error.
	cause error
	calls []string
	live  map[string]bool
}

func (r *recorder) record(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	nth := 1
	for _, call := range r.calls {
		if call == name {
			nth++
		}
	}
	r.calls = append(r.calls, name)

	if slices.Contains(r.fail, name) || slices.Contains(r.fail, fmt.Sprintf("%s#%d", name, nth)) {
		if r.cause != nil {
			return fmt.Errorf("forced failure at %s: %w", name, r.cause)
		}

		return fmt.Errorf("forced failure at %s", name)
	}

	return nil
}

// cleanup also notes whether the teardown got a context the interrupt had not already cancelled.
func (r *recorder) cleanup(ctx context.Context, name string) error {
	r.mu.Lock()
	r.live[name] = ctx.Err() == nil
	r.mu.Unlock()

	return r.record(name)
}

// snapshot copies the recorded calls under the lock, for a reader that races a still-running goroutine.
func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.calls)
}

// fakeDigest is what every image the fake holds is pinned at.
const fakeDigest = "sha256:0a1b"

type fakeImages struct {
	r *recorder
	// gone says the store holds no image, as after an rm --force.
	gone bool
}

func (f fakeImages) Pull(_ context.Context, ref string) (image.Image, error) {
	if err := f.r.record("images.Pull"); err != nil {
		return image.Image{}, err
	}

	return cachedImage(ref), nil
}

func (f fakeImages) Lookup(ref string) (image.Image, bool, error) {
	if err := f.r.record("images.Lookup"); err != nil {
		return image.Image{}, false, err
	}
	if f.gone {
		return image.Image{}, false, nil
	}

	return cachedImage(ref), true, nil
}

// cachedImage names a command of its own, which a create must never run.
func cachedImage(ref string) image.Image {
	return image.Image{Reference: ref, Digest: fakeDigest, RootFS: "/images/alpine", Config: models.ImageConfig{Entrypoint: []string{"/bin/sh"}, Cmd: []string{"-c", "exit 1"}}}
}

// stalledImages holds every pull until its context ends, the way a registry that never answers does.
type stalledImages struct {
	r       *recorder
	entered chan struct{}
}

func (f stalledImages) Pull(ctx context.Context, _ string) (image.Image, error) {
	if err := f.r.record("images.Pull"); err != nil {
		return image.Image{}, err
	}
	close(f.entered)
	<-ctx.Done()

	return image.Image{}, ctx.Err()
}

func (f stalledImages) Lookup(string) (image.Image, bool, error) { return image.Image{}, false, nil }

// fakeRepo holds one record, so a test says what it held before the verb ran and reads what it holds after.
type fakeRepo struct {
	r  *recorder
	sb models.Sandbox
	// left is what List answers with.
	left []models.Sandbox
	// listErr is the non-fatal error List returns beside left, for the unreadable-record path.
	listErr error
	missing bool
	deleted bool
	// created is the record as Create was handed it, so a test says what the request put in it.
	created models.Sandbox
	// made is the record a fork created, which lives beside the source the test set up.
	made *models.Sandbox
	// checkpointDir and stateDir replace the fixed paths when a test needs the directory to exist on disk.
	checkpointDir string
	stateDir      string
	// onGet runs inside every Get, so a test moves the record on the goroutine that polls it.
	onGet func()
	// onCreate runs once a fork's copy exists, so a test lands a verb before the fork takes its lock.
	onCreate func(id string)
	// unmade is a fork's copy that a delete took, which reads not found from then on.
	unmade string
}

func (f *fakeRepo) Get(id string) (models.Sandbox, error) {
	if err := f.r.record("repo.Get"); err != nil {
		return models.Sandbox{}, err
	}
	if f.onGet != nil {
		f.onGet()
	}
	if f.made != nil && id == f.made.ID {
		return *f.made, nil
	}
	if f.missing || id != f.sb.ID {
		return models.Sandbox{}, fmt.Errorf("sandbox %s: %w", id, sandboxstate.ErrNotFound)
	}

	return f.sb, nil
}

func (f *fakeRepo) Resolve(ref string) (string, error) {
	if f.sb.Name != "" && ref == f.sb.Name {
		return f.sb.ID, nil
	}

	return ref, nil
}

func (f *fakeRepo) List() ([]models.Sandbox, error) {
	if err := f.r.record("repo.List"); err != nil {
		return nil, err
	}

	return f.left, f.listErr
}

func (f *fakeRepo) Create(sb models.Sandbox, admit ...func(dir string) error) (models.Sandbox, error) {
	// The repository runs each admission on the claimed directory, before it writes the record.
	for _, check := range admit {
		if err := check("/sandboxes/sandbox1"); err != nil {
			return models.Sandbox{}, err
		}
	}
	if err := f.r.record("repo.Create"); err != nil {
		return models.Sandbox{}, err
	}

	sb.ID = "sandbox1"
	f.created = sb

	// A fork creates beside the source the test set up, so the copy takes the second id.
	if f.sb.ID != "" {
		sb.ID = "sandbox2"
		f.made = &sb
		if f.onCreate != nil {
			f.onCreate(sb.ID)
		}

		return sb, nil
	}
	f.sb = sb

	return sb, nil
}

func (f *fakeRepo) Update(id string, mutate func(*models.Sandbox) error) error {
	if err := f.r.record("repo.Update"); err != nil {
		return err
	}
	if f.made != nil && id == f.made.ID {
		return mutate(f.made)
	}
	if id != f.sb.ID {
		return fmt.Errorf("sandbox %s: %w", id, sandboxstate.ErrNotFound)
	}

	return mutate(&f.sb)
}

// CheckpointDir is where a pause writes and a fork reads. It is not created until one happens.
func (f *fakeRepo) CheckpointDir(id string) (string, error) {
	if err := f.r.record("repo.CheckpointDir"); err != nil {
		return "", err
	}
	if f.checkpointDir != "" {
		return f.checkpointDir, nil
	}

	return "/checkpoints/" + id, nil
}

func (f *fakeRepo) Dir(id string) (string, error) {
	if err := f.r.record("repo.Dir"); err != nil {
		return "", err
	}
	if f.stateDir != "" {
		return f.stateDir, nil
	}

	return "/state/" + id, nil
}

func (f *fakeRepo) Delete(id string) error {
	if err := f.r.record("repo.Delete"); err != nil {
		return err
	}
	if id == f.unmade {
		return fmt.Errorf("sandbox %s: %w", id, sandboxstate.ErrNotFound)
	}
	if f.made != nil && id == f.made.ID {
		f.unmade, f.made = id, nil
	}
	f.deleted = true

	return nil
}

type fakeNet struct {
	r *recorder
	// allocateErr is what Allocate answers with, so a test can hand create the pool's own refusal.
	allocateErr error
	allocated   bool
	released    bool
}

func (f *fakeNet) Allocate(_ context.Context, id string) (models.NetworkSpec, error) {
	if err := f.r.record("net.Allocate"); err != nil {
		return models.NetworkSpec{}, err
	}
	if f.allocateErr != nil {
		return models.NetworkSpec{}, f.allocateErr
	}
	f.allocated = true

	return models.NetworkSpec{
		NetnsPath:     "/run/netns/" + id,
		Address:       netip.MustParsePrefix("10.0.0.2/24"),
		Gateway:       netip.MustParseAddr("10.0.0.1"),
		HostInterface: "shardv2",
		Nameservers:   []netip.Addr{netip.MustParseAddr("1.1.1.1")},
	}, nil
}

func (f *fakeNet) Release(ctx context.Context, _ string) error {
	if err := f.r.cleanup(ctx, "net.Release"); err != nil {
		return err
	}
	f.released = true

	return nil
}

// Reapply is reached by a create with a policy only: without one Allocate applied the rules over the fresh netns.
func (f *fakeNet) Reapply(context.Context, string) error {
	return f.r.record("net.Reapply")
}

func (f *fakeNet) ReapplyAll(context.Context) error {
	return f.r.record("net.ReapplyAll")
}

type fakeProvider struct {
	models.Provider

	r *recorder
	// mu guards the fields Exec and Signal write, so two concurrent execs never race on them.
	mu     sync.Mutex
	status models.Status
	exit   models.ExitStatus
	// entrypointExit is what the non-blocking ExitStatus reads: nil while the entrypoint still runs.
	entrypointExit *models.ExitStatus
	// entrypointErr is what ExitStatus answers instead, as a guest that replaced the exit channel makes it.
	entrypointErr error
	// waitErr is what a sandbox the stop had to kill answers with: it recorded no exit status.
	waitErr error
	// failsOnStop is the reason a shard-init that dies on the way down gives, which the stopped status carries.
	failsOnStop string
	// restarts is what the supervisor counted on this run, and restartsErr a count file that cannot be read.
	restarts    models.RestartCount
	restartsErr error
	// appEnds runs on the second Restarts, the way shard-init ends the app while a run waits on it.
	appEnds       func()
	restartsCalls int
	// stopApps is the force of every StopApp, in order.
	stopApps []bool
	// onRemove runs inside Remove, so a test can say what the host looks like during a teardown.
	onRemove func()
	// onFork runs inside Fork, so a test can say what a fork that fails left on the host.
	onFork func()
	// gate, when set, holds Start until it is closed, so a test can put a second verb behind it.
	gate <-chan struct{}
	// entered is closed the first time Start is reached.
	entered chan struct{}
	// stopGate holds Stop the way gate holds Start, and stopEntered is closed when Stop is reached.
	stopGate    <-chan struct{}
	stopEntered chan struct{}
	// wedgeStartOf is the id whose Start hangs until the caller's deadline, the way a wedged runtime does.
	wedgeStartOf string
	// statusGate, when set, holds Status until it is closed or the context ends, so a test wedges the substrate.
	statusGate chan struct{}
	// stopUnwedges makes Stop close statusGate, the way a kill frees a substrate a Status call had wedged.
	stopUnwedges bool
	// reclaimed says the raw kill ran, and reclaimErr is a kill that did not land.
	reclaimed  bool
	reclaimErr error

	// refuse is what CheckResources answers, the way vz refuses a --memory it cannot boot under.
	refuse error
	// startErr is what Start answers once it recorded the call, as a substrate whose app never started does.
	startErr error
	// spec is what Create was handed, so a test says what reached the substrate.
	spec    models.SandboxSpec
	grace   time.Duration
	started bool
	stopped bool
	removed bool

	// noPause, noResume, noFork and noPort withhold one optional verb each, which the fake otherwise claims.
	noPause  bool
	noResume bool
	noFork   bool
	noPort   bool
	// pauseErr is what Pause refuses with, the way vz refuses a pause into a silent shim.
	pauseErr error
	// createErr and forkErr are what Create and Fork refuse with, the way a VM substrate refuses a disk.
	createErr error
	forkErr   error
	// checkpointDir is the directory the pause was told to write into, and the one the resume read.
	checkpointDir string
	// forkedFrom is the running source the fork was told to capture.
	forkedFrom string
	// source is the stopped sandbox the snapshot was told to copy.
	source  string
	paused  bool
	resumed bool
	// lose makes the pause end the sandbox the way a checkpoint that broke off does.
	lose bool
	// pauseCtxErr is what the pause's context said when the pause began, so a test sees a client's cancel.
	pauseCtxErr error
	// spendBudget makes the pause write its checkpoint and then wait out its deadline, the way a wedged delete does.
	spendBudget bool
	// cleanupFails makes the pause write its checkpoint and then fail the delete, with the sentry gone.
	cleanupFails bool
	// cleanupFreezes makes the pause write its checkpoint and then fail the delete, with the sentry still frozen.
	cleanupFreezes bool
	// mounted is a merged view runsc no longer holds: Stop refuses it the way the gVisor orphan guard does, and only Release frees it.
	mounted bool

	// logPath is the file the output is read from, which a test writes into.
	logPath string
	// aliveAfterStop is the substrate reporting a stopped sandbox alive that many more times, which is
	// the race stop waits out. A negative count never settles.
	aliveAfterStop int
	// exits runs on the second Status, and the sandbox is gone from that call on.
	exits       func()
	statusCalls int
	// onStatus runs inside every Status, so a test lands what a verb that holds no lock against the caller writes meanwhile.
	onStatus func()
	// execOut and execErrOut are what the command writes on each stream, and execInput what it read.
	execOut    string
	execErrOut string
	execInput  string
	execExit   models.ExitStatus
	execErr    error
	execID     string
	execSpec   models.ExecSpec
	// execPID is the guest pid the fake reports, so a kill has a process to wait for and to signal.
	execPID int
	// execNoPID makes Exec report no pid, the way a command the substrate never started reports none.
	execNoPID bool
	// execBegan is closed when a command starts, and execWaits holds it there until the test closes it.
	execBegan chan struct{}
	execWaits chan struct{}
	// signaled is closed by Signal, so a held command ends the way a real signal ends one.
	signaled  chan struct{}
	signalPID int
	signalGot string
	signalErr error
	// serve, when set, answers the exec in place of the canned streams, the way shard-init's files mode does.
	serve func(spec models.ExecSpec) (models.ExitStatus, error)
	// execCtx is what the last exec ran on, so a test sees whether the exec outlives its request.
	execCtx context.Context
}

func (f *fakeProvider) LogPath(string) (string, error) {
	if err := f.r.record("provider.LogPath"); err != nil {
		return "", err
	}

	return f.logPath, nil
}

func (f *fakeProvider) HeldLogs(string) ([]string, error) {
	return nil, nil
}

func (f *fakeProvider) Exec(ctx context.Context, id string, spec models.ExecSpec) (models.ExitStatus, error) {
	if err := f.r.record("provider.Exec"); err != nil {
		return models.ExitStatus{}, err
	}
	f.mu.Lock()
	f.execID, f.execSpec, f.execCtx = id, spec, ctx
	f.mu.Unlock()

	if f.serve != nil {
		return f.serve(spec)
	}

	if spec.Report != nil && !f.execNoPID {
		spec.Report(f.execPID)
	}

	if f.execBegan != nil {
		close(f.execBegan)
	}

	// A terminal's replica never reaches EOF until the master closes, which the daemon does only once
	// this call returns, so a tty command reads nothing here.
	if spec.Stdin != nil && !spec.TTY {
		read, err := io.ReadAll(spec.Stdin)
		if err != nil {
			return models.ExitStatus{}, err
		}
		f.mu.Lock()
		f.execInput = string(read)
		f.mu.Unlock()
	}

	// The command emits its output, then runs on until the test releases or signals it, so a client
	// that drops mid-command and re-attaches sees the same bytes replayed.
	if f.execOut != "" {
		if _, err := spec.Stdout.WriteString(f.execOut); err != nil {
			return models.ExitStatus{}, err
		}
	}
	if f.execErrOut != "" {
		if _, err := spec.Stderr.WriteString(f.execErrOut); err != nil {
			return models.ExitStatus{}, err
		}
	}

	// A held command ends when the caller gives up, when the test releases it, or when a signal reaches it.
	if f.execWaits != nil || f.signaled != nil {
		select {
		case <-f.execWaits:
		case <-f.signaled:
		case <-ctx.Done():
			return models.ExitStatus{}, ctx.Err()
		}
	}

	return f.execExit, f.execErr
}

func (f *fakeProvider) Signal(_ context.Context, _ string, pid int, signal string) error {
	if err := f.r.record("provider.Signal"); err != nil {
		return err
	}
	if f.signalErr != nil {
		return f.signalErr
	}
	f.mu.Lock()
	f.signalPID, f.signalGot = pid, signal
	f.mu.Unlock()

	if f.signaled != nil {
		close(f.signaled)
	}

	return nil
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) CheckResources(models.Resources) error { return f.refuse }

func (f *fakeProvider) Capabilities() models.Capabilities {
	return models.Capabilities{Pause: !f.noPause, Resume: !f.noResume, Fork: !f.noFork, Port: !f.noPort}
}

func (f *fakeProvider) Pause(ctx context.Context, id string, dir string) error {
	f.pauseCtxErr = ctx.Err()
	if err := f.r.record("provider.Pause"); err != nil {
		return err
	}
	if f.pauseErr != nil {
		return f.pauseErr
	}
	if f.lose {
		f.status = models.Status{}

		return &models.LostError{Sandbox: id, Err: fmt.Errorf("checkpoint sandbox %s: no space left on device", id)}
	}
	if f.spendBudget || f.cleanupFails || f.cleanupFreezes {
		if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
			return err
		}
	}
	if f.cleanupFreezes {
		f.status = frozen()

		return fmt.Errorf("delete sandbox %s after its checkpoint: device or resource busy", id)
	}
	if f.cleanupFails {
		f.status = models.Status{}

		return fmt.Errorf("delete sandbox %s after its checkpoint: device or resource busy", id)
	}
	if f.spendBudget {
		<-ctx.Done()
		f.status = models.Status{}

		return fmt.Errorf("delete sandbox %s after its checkpoint: %w", id, ctx.Err())
	}
	f.paused, f.checkpointDir = true, dir
	f.status = models.Status{Exists: true, State: models.StatePaused}

	return nil
}

func (f *fakeProvider) Resume(_ context.Context, _ string, dir string) error {
	if err := f.r.record("provider.Resume"); err != nil {
		return err
	}
	f.resumed, f.checkpointDir = true, dir
	f.status = models.Status{Exists: true, State: models.StateRunning, PID: 7}

	return nil
}

func (f *fakeProvider) Fork(_ context.Context, source string, spec models.SandboxSpec) error {
	if f.onFork != nil {
		f.onFork()
	}
	if err := f.r.record("provider.Fork"); err != nil {
		return err
	}
	if f.forkErr != nil {
		return f.forkErr
	}
	f.spec, f.forkedFrom = spec, source
	f.status = models.Status{Exists: true, State: models.StateRunning, PID: 7}

	return nil
}

func (f *fakeProvider) AdoptStaging(string) error { return nil }

func (f *fakeProvider) Snapshot(_ context.Context, source, dir string) error {
	if err := f.r.record("provider.Snapshot"); err != nil {
		return err
	}
	f.source = source

	return os.WriteFile(filepath.Join(dir, "upper"), []byte("kept"), 0o600)
}

func (f *fakeProvider) Create(_ context.Context, spec models.SandboxSpec) error {
	f.spec = spec
	if err := f.r.record("provider.Create"); err != nil {
		return err
	}

	return f.createErr
}

func (f *fakeProvider) Start(ctx context.Context, id string) error {
	if f.entered != nil {
		close(f.entered)
		f.entered = nil
	}
	// A wedged runtime's Start hangs on its own runsc state; only the caller's deadline ends it.
	if f.wedgeStartOf != "" && id == f.wedgeStartOf {
		<-ctx.Done()

		return ctx.Err()
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := f.r.record("provider.Start"); err != nil {
		return err
	}
	if f.startErr != nil {
		return f.startErr
	}
	f.started = true
	f.status = models.Status{Exists: true, State: models.StateRunning, PID: 7}

	return nil
}

func (f *fakeProvider) Stop(ctx context.Context, _ string, grace time.Duration) error {
	if f.stopEntered != nil {
		close(f.stopEntered)
	}
	if f.stopGate != nil {
		select {
		case <-f.stopGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := f.r.record("provider.Stop"); err != nil {
		return err
	}
	if f.mounted && !f.status.Exists {
		return errors.New("runsc does not hold sandbox sandbox1 but its rootfs is still mounted")
	}
	f.stopped, f.grace = true, grace
	if f.aliveAfterStop == 0 {
		f.status = models.Status{Exists: true, State: models.StateStopped, SupervisorFailed: f.failsOnStop}
	}
	if f.stopUnwedges && f.statusGate != nil {
		close(f.statusGate)
		f.statusGate = nil
	}

	return nil
}

// Release frees what a cut pause left beside its checkpoint, the frozen sentry and the merged view alike.
func (f *fakeProvider) Release(_ context.Context, _, _ string) error {
	if err := f.r.record("provider.Release"); err != nil {
		return err
	}
	f.status, f.mounted = models.Status{}, false

	return nil
}

// frozen is a sentry a pause froze and never deleted, which runsc still reports paused and alive.
func frozen() models.Status {
	return models.Status{Exists: true, State: models.StatePaused, PID: 42}
}

// Reclaim is the raw kill a wedged substrate gets, and it frees the substrate the way Stop's kill does.
func (f *fakeProvider) Reclaim(_ context.Context, _ string) error {
	if err := f.r.record("provider.Reclaim"); err != nil {
		return err
	}
	if f.reclaimErr != nil {
		return f.reclaimErr
	}
	f.reclaimed = true
	if f.statusGate != nil {
		close(f.statusGate)
		f.statusGate = nil
	}

	return nil
}

func (f *fakeProvider) Remove(ctx context.Context, _ string) error {
	if f.onRemove != nil {
		f.onRemove()
	}
	if err := f.r.cleanup(ctx, "provider.Remove"); err != nil {
		return err
	}
	f.removed = true

	return nil
}

func (f *fakeProvider) Status(ctx context.Context, _ string) (models.Status, error) {
	// A real provider runs its probe under ctx, so after a wedged pause a done ctx fails it at once.
	if err := ctx.Err(); f.spendBudget && err != nil {
		return models.Status{}, fmt.Errorf("status: %w", err)
	}
	if f.statusGate != nil {
		select {
		case <-f.statusGate:
		case <-ctx.Done():
			return models.Status{}, ctx.Err()
		}
	}
	if err := f.r.record("provider.Status"); err != nil {
		return models.Status{}, err
	}
	if f.onStatus != nil {
		f.onStatus()
	}

	if f.stopped && f.aliveAfterStop != 0 {
		f.aliveAfterStop--
		if f.aliveAfterStop == 0 {
			f.status = models.Status{Exists: true, State: models.StateStopped}
		}

		return models.Status{Exists: true, State: models.StateRunning}, nil
	}

	// exits is the sandbox writing its last line and going, which happens while a follow is up.
	f.statusCalls++
	if f.exits != nil && f.statusCalls == 2 {
		f.exits()
		f.status = models.Status{}
	}

	return f.status, nil
}

func (f *fakeProvider) Restarts(context.Context, string) (models.RestartCount, error) {
	if err := f.r.record("provider.Restarts"); err != nil {
		return models.RestartCount{}, err
	}
	if f.restartsErr != nil {
		return models.RestartCount{}, f.restartsErr
	}
	f.restartsCalls++
	if f.appEnds != nil && f.restartsCalls == 2 {
		f.appEnds()
	}

	return f.restarts, nil
}

func (f *fakeProvider) StopApp(_ context.Context, _ string, force bool) error {
	if err := f.r.record("provider.StopApp"); err != nil {
		return err
	}
	f.stopApps = append(f.stopApps, force)

	return nil
}

func (f *fakeProvider) Wait(context.Context, string) (models.ExitStatus, error) {
	if err := f.r.record("provider.Wait"); err != nil {
		return models.ExitStatus{}, err
	}
	if f.waitErr != nil {
		return models.ExitStatus{}, f.waitErr
	}

	return f.exit, nil
}

func (f *fakeProvider) ExitStatus(context.Context, string) (*models.ExitStatus, error) {
	if err := f.r.record("provider.ExitStatus"); err != nil {
		return nil, err
	}
	if f.entrypointErr != nil {
		return nil, f.entrypointErr
	}

	return f.entrypointExit, nil
}

// fakeSubstrate stands in for the runtime root, which off Linux has no mount to give back.
type fakeSubstrate struct {
	r       *recorder
	dropped bool
}

func (f *fakeSubstrate) ReleaseRoot() error {
	if err := f.r.record("substrate.ReleaseRoot"); err != nil {
		return err
	}
	f.dropped = true

	return nil
}

// fakePorts stands in for the host listeners: a test says which address the host refuses, and reads which forwards are up.
type fakePorts struct {
	mu    sync.Mutex
	calls []string
	// up is the sandbox each listening host port carries to, and specs what it carries.
	up    map[uint16]string
	specs map[uint16]models.PortForward
	// refuse is why the host will not listen on an address, as 127.0.0.1:8080.
	refuse map[string]error
	// down is what Status says of a host port the host refused.
	down map[uint16]portforward.Status
}

func newFakePorts() *fakePorts {
	return &fakePorts{up: map[uint16]string{}, specs: map[uint16]models.PortForward{}, refuse: map[string]error{}, down: map[uint16]portforward.Status{}}
}

func (f *fakePorts) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakePorts) bind(id string, spec models.PortForward) error {
	delete(f.up, spec.HostPort)
	if err := f.refused(spec); err != nil {
		f.down[spec.HostPort] = portforward.Status{Sandbox: id, Error: err.Public()}

		return err
	}
	f.up[spec.HostPort], f.specs[spec.HostPort] = id, spec
	delete(f.down, spec.HostPort)

	return nil
}

func (f *fakePorts) refused(spec models.PortForward) *portforward.BindError {
	err, ok := f.refuse[fmt.Sprintf("%s:%d", portforward.Address(spec.Public), spec.HostPort)]
	if !ok {
		return nil
	}

	return &portforward.BindError{Port: spec.HostPort, Err: err}
}

func (f *fakePorts) Open(id string, spec models.PortForward) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Open %s %d", id, spec.HostPort)

	return f.bind(id, spec)
}

// Set leaves a refused port down and errs on none, the way the forwarder keeps the refusal for ls.
func (f *fakePorts) Set(id string, want []models.PortForward) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Set %s %d", id, len(want))

	for port, owner := range f.up {
		if owner == id && !slices.ContainsFunc(want, func(p models.PortForward) bool { return p.HostPort == port }) {
			delete(f.up, port)
		}
	}
	for _, spec := range want {
		if err := f.bind(id, spec); err != nil {
			f.record("refused %d", spec.HostPort)
		}
	}

	return nil
}

func (f *fakePorts) Close(id string, hostPort uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Close %s %d", id, hostPort)

	if f.up[hostPort] == id {
		delete(f.up, hostPort)
	}

	return nil
}

func (f *fakePorts) CloseSandbox(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CloseSandbox %s", id)

	for port, owner := range f.up {
		if owner == id {
			delete(f.up, port)
		}
	}

	return nil
}

func (f *fakePorts) Sandboxes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var ids []string
	for _, owner := range f.up {
		if !slices.Contains(ids, owner) {
			ids = append(ids, owner)
		}
	}
	slices.Sort(ids)

	return ids
}

func (f *fakePorts) Probe(spec models.PortForward) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Probe %d", spec.HostPort)

	if err := f.refused(spec); err != nil {
		return err
	}

	return nil
}

func (f *fakePorts) Status(hostPort uint16) portforward.Status {
	f.mu.Lock()
	defer f.mu.Unlock()

	if status, ok := f.down[hostPort]; ok {
		return status
	}
	id, ok := f.up[hostPort]

	return portforward.Status{Sandbox: id, Listening: ok}
}

func (f *fakePorts) Reachable(public bool) ([]models.HostAddress, error) {
	on := []models.HostAddress{{Interface: "lo", Address: "127.0.0.1"}}
	if public {
		on = append(on, models.HostAddress{Interface: "eth0", Address: "192.0.2.10"})
	}

	return on, nil
}

// listening is the host ports up for sandbox id, in order.
func (f *fakePorts) listening(id string) []uint16 {
	f.mu.Lock()
	defer f.mu.Unlock()

	var ports []uint16
	for port, owner := range f.up {
		if owner == id {
			ports = append(ports, port)
		}
	}
	slices.Sort(ports)

	return ports
}

// layers is every fake the service was built over, for a test to set up and read back.
type layers struct {
	repo      *fakeRepo
	net       *fakeNet
	provider  *fakeProvider
	substrate *fakeSubstrate
	ports     *fakePorts
	secrets   *secret.Store
	policies  *egress.Store
	snapshots *sandboxstate.Snapshots
}

// newService wires the orchestrator onto fakes and the two file stores, over the one record sb.
func newService(t *testing.T, r *recorder, sb models.Sandbox, tune ...func(*sandbox.Config)) (*sandbox.Service, layers) {
	t.Helper()

	r.live = map[string]bool{}
	root := t.TempDir()

	secrets, err := secret.New(filepath.Join(root, "secrets"), nil)
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}

	policies, err := egress.NewStore(filepath.Join(root, "policies"))
	if err != nil {
		t.Fatalf("egress.NewStore: %v", err)
	}

	snapshots, err := sandboxstate.NewSnapshots(filepath.Join(root, "state"))
	if err != nil {
		t.Fatalf("sandboxstate.NewSnapshots: %v", err)
	}

	// A record that is not there yet is what a create sees, and the substrate then reports the fresh sandbox.
	status := models.Status{Exists: true, State: models.StateCreated, PID: 42}
	if sb.ID != "" {
		status = models.Status{Exists: true, State: sb.State}
	}

	l := layers{
		repo:      &fakeRepo{r: r, sb: sb},
		net:       &fakeNet{r: r},
		provider:  &fakeProvider{r: r, status: status},
		substrate: &fakeSubstrate{r: r},
		ports:     newFakePorts(),
		secrets:   secrets,
		policies:  policies,
		snapshots: snapshots,
	}

	cfg := sandbox.Config{
		Repo:      l.repo,
		Snapshots: snapshots,
		Images:    fakeImages{r: r},
		Network:   l.net,
		Provider:  l.provider,
		Secrets:   secrets,
		Policies:  policies,
		Substrate: l.substrate,
		Ports:     l.ports,
		// The fakes build a real bundle where a grant is under test, so the environment is the bundle's.
		Environments: bundle.Opener(l.repo.Dir),
		ProxyCA: func() ([]byte, error) {
			ca, err := proxy.LoadCA(filepath.Join(root, "proxy"))
			if err != nil {
				return nil, err
			}

			return ca.CertPEM(), nil
		},
		PullTimeout: time.Minute,
	}
	for _, apply := range tune {
		apply(&cfg)
	}

	return sandbox.New(cfg), l
}

// leased is the address every create takes before its start, so a live record always holds one.
var leased = netip.MustParsePrefix("10.0.0.2/24")

// running is the record of a sandbox that is up, which is what stop and rm are given in most tests.
func running() models.Sandbox {
	return models.Sandbox{ID: "sandbox1", State: models.StateRunning, PID: 42, Address: leased}
}

// forkSource is a running sandbox whose entrypoint already exited, which a fork captures as it is.
func forkSource() models.Sandbox {
	return models.Sandbox{ID: "sandbox1", Name: "web", State: models.StateRunning, PID: 42, ExitStatus: &models.ExitStatus{Code: 3}}
}

// pausedSandbox is a sandbox that holds a checkpoint, which is what resume and fork are given.
func pausedSandbox() models.Sandbox {
	return models.Sandbox{ID: "sandbox1", Name: "web", State: models.StatePaused, Checkpoint: "/checkpoints/sandbox1",
		ExitStatus: &models.ExitStatus{Code: 3}}
}

func stopped() models.Sandbox {
	return models.Sandbox{ID: "sandbox1", Name: "web", State: models.StateStopped, ExitStatus: &models.ExitStatus{Code: 3}}
}

// keep filters the calls down to the named ones, in the order they happened.
func keep(calls []string, names ...string) []string {
	var kept []string
	for _, call := range calls {
		if slices.Contains(names, call) {
			kept = append(kept, call)
		}
	}

	return kept
}
