package sandbox_test

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/secret"
)

// recorder logs what the fakes were asked in order; a name in fail fails every call, a name#N the Nth only.
type recorder struct {
	fail  []string
	calls []string
	live  map[string]bool
}

func (r *recorder) record(name string) error {
	nth := 1
	for _, call := range r.calls {
		if call == name {
			nth++
		}
	}
	r.calls = append(r.calls, name)

	if slices.Contains(r.fail, name) || slices.Contains(r.fail, fmt.Sprintf("%s#%d", name, nth)) {
		return fmt.Errorf("forced failure at %s", name)
	}

	return nil
}

// cleanup also notes whether the teardown got a context the interrupt had not already cancelled.
func (r *recorder) cleanup(ctx context.Context, name string) error {
	r.live[name] = ctx.Err() == nil

	return r.record(name)
}

type fakeImages struct {
	r *recorder
}

func (f fakeImages) Pull(context.Context, string) (image.Image, error) {
	if err := f.r.record("images.Pull"); err != nil {
		return image.Image{}, err
	}

	return image.Image{Reference: "alpine:3.20", RootFS: "/images/alpine"}, nil
}

// fakeRepo holds one record, so a test says what it held before the verb ran and reads what it holds after.
type fakeRepo struct {
	r  *recorder
	sb models.Sandbox
	// left is what List answers with.
	left    []models.Sandbox
	missing bool
	deleted bool
	// created is the record as Create was handed it, so a test says what the request put in it.
	created models.Sandbox
	// made is the record a fork or a clone created, which lives beside the source the test set up.
	made *models.Sandbox
	// snapshotDir replaces the fixed path when a test needs the directory to exist on disk.
	snapshotDir string
}

func (f *fakeRepo) Get(id string) (models.Sandbox, error) {
	if err := f.r.record("repo.Get"); err != nil {
		return models.Sandbox{}, err
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

	return f.left, nil
}

func (f *fakeRepo) Create(sb models.Sandbox) (models.Sandbox, error) {
	if err := f.r.record("repo.Create"); err != nil {
		return models.Sandbox{}, err
	}

	sb.ID = "sandbox1"
	f.created = sb

	// A fork and a clone create beside the source the test set up, so the copy takes the second id.
	if f.sb.ID != "" {
		sb.ID = "sandbox2"
		f.made = &sb

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

// SnapshotDir is where a pause writes and a fork reads. It is not created until one happens.
func (f *fakeRepo) SnapshotDir(id string) (string, error) {
	if err := f.r.record("repo.SnapshotDir"); err != nil {
		return "", err
	}
	if f.snapshotDir != "" {
		return f.snapshotDir, nil
	}

	return "/snapshots/" + id, nil
}

func (f *fakeRepo) Dir(id string) (string, error) {
	if err := f.r.record("repo.Dir"); err != nil {
		return "", err
	}

	return "/state/" + id, nil
}

func (f *fakeRepo) Delete(string) error {
	if err := f.r.record("repo.Delete"); err != nil {
		return err
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

	return models.NetworkSpec{NetnsPath: "/run/netns/" + id, Address: netip.MustParsePrefix("10.0.0.2/24"), HostInterface: "shardv2"}, nil
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

	r      *recorder
	status models.Status
	exit   models.ExitStatus
	// waitErr is what a sandbox the stop had to kill answers with: it recorded no exit status.
	waitErr error
	// onRemove runs inside Remove, so a test can say what the host looks like during a teardown.
	onRemove func()
	// gate, when set, holds Start until it is closed, so a test can put a second verb behind it.
	gate <-chan struct{}
	// entered is closed the first time Start is reached.
	entered chan struct{}

	// spec is what Create was handed, so a test says what reached the substrate.
	spec    models.SandboxSpec
	grace   time.Duration
	started bool
	stopped bool
	removed bool

	// noPause, noResume and noFork withhold one optional verb each, which the fake otherwise claims.
	noPause  bool
	noResume bool
	noFork   bool
	// snapshotDir is the directory the pause was told to write into, and the one the fork read.
	snapshotDir string
	// source is the sandbox the clone was told to copy.
	source  string
	paused  bool
	resumed bool

	// logPath is the file the output is read from, which a test writes into.
	logPath string
	// exits runs on the second Status, and the sandbox is gone from that call on.
	exits       func()
	statusCalls int
	// execOut and execErrOut are what the command writes on each stream, and execInput what it read.
	execOut    string
	execErrOut string
	execInput  string
	execExit   models.ExitStatus
	execErr    error
	execID     string
	execSpec   models.ExecSpec
}

func (f *fakeProvider) LogPath(string) (string, error) {
	if err := f.r.record("provider.LogPath"); err != nil {
		return "", err
	}

	return f.logPath, nil
}

func (f *fakeProvider) Exec(_ context.Context, id string, spec models.ExecSpec) (models.ExitStatus, error) {
	if err := f.r.record("provider.Exec"); err != nil {
		return models.ExitStatus{}, err
	}
	f.execID, f.execSpec = id, spec

	if spec.Stdin != nil {
		read, err := io.ReadAll(spec.Stdin)
		if err != nil {
			return models.ExitStatus{}, err
		}
		f.execInput = string(read)
	}

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

	return f.execExit, f.execErr
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Capabilities() models.Capabilities {
	return models.Capabilities{Pause: !f.noPause, Resume: !f.noResume, Fork: !f.noFork}
}

func (f *fakeProvider) Pause(_ context.Context, _ string, dir string) error {
	if err := f.r.record("provider.Pause"); err != nil {
		return err
	}
	f.paused, f.snapshotDir = true, dir
	f.status = models.Status{Exists: true, State: models.StatePaused}

	return nil
}

func (f *fakeProvider) Resume(_ context.Context, _ string, dir string) error {
	if err := f.r.record("provider.Resume"); err != nil {
		return err
	}
	f.resumed, f.snapshotDir = true, dir
	f.status = models.Status{Exists: true, State: models.StateRunning, PID: 7}

	return nil
}

func (f *fakeProvider) Fork(_ context.Context, dir string, spec models.SandboxSpec) error {
	if err := f.r.record("provider.Fork"); err != nil {
		return err
	}
	f.spec, f.snapshotDir = spec, dir
	f.status = models.Status{Exists: true, State: models.StateRunning, PID: 7}

	return nil
}

func (f *fakeProvider) Clone(_ context.Context, source string, spec models.SandboxSpec) error {
	if err := f.r.record("provider.Clone"); err != nil {
		return err
	}
	f.spec, f.source = spec, source
	f.status = models.Status{Exists: true, State: models.StateRunning, PID: 7}

	return nil
}

func (f *fakeProvider) Create(_ context.Context, spec models.SandboxSpec) error {
	f.spec = spec

	return f.r.record("provider.Create")
}

func (f *fakeProvider) Start(context.Context, string) error {
	if f.entered != nil {
		close(f.entered)
		f.entered = nil
	}
	if f.gate != nil {
		<-f.gate
	}
	if err := f.r.record("provider.Start"); err != nil {
		return err
	}
	f.started = true
	f.status = models.Status{Exists: true, State: models.StateRunning, PID: 7}

	return nil
}

func (f *fakeProvider) Stop(_ context.Context, _ string, grace time.Duration) error {
	if err := f.r.record("provider.Stop"); err != nil {
		return err
	}
	f.stopped, f.grace = true, grace
	f.status = models.Status{Exists: true, State: models.StateStopped}

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

func (f *fakeProvider) Status(context.Context, string) (models.Status, error) {
	if err := f.r.record("provider.Status"); err != nil {
		return models.Status{}, err
	}

	// exits is the sandbox writing its last line and going, which happens while a follow is up.
	f.statusCalls++
	if f.exits != nil && f.statusCalls == 2 {
		f.exits()
		f.status = models.Status{}
	}

	return f.status, nil
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

// fakeSubstrate stands in for the runsc root, which off Linux has no mount to give back.
type fakeSubstrate struct {
	r       *recorder
	dropped bool
}

func (f *fakeSubstrate) DropNullNetns() error {
	if err := f.r.record("substrate.DropNullNetns"); err != nil {
		return err
	}
	f.dropped = true

	return nil
}

// layers is every fake the service was built over, for a test to set up and read back.
type layers struct {
	repo      *fakeRepo
	net       *fakeNet
	provider  *fakeProvider
	substrate *fakeSubstrate
	secrets   *secret.Store
	policies  *egress.Store
}

// newService wires the orchestrator onto fakes and the two file stores, over the one record sb.
func newService(t *testing.T, r *recorder, sb models.Sandbox) (*sandbox.Service, layers) {
	t.Helper()

	r.live = map[string]bool{}
	root := t.TempDir()

	secrets, err := secret.New(filepath.Join(root, "secrets"))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}

	policies, err := egress.NewStore(filepath.Join(root, "policies"))
	if err != nil {
		t.Fatalf("egress.NewStore: %v", err)
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
		secrets:   secrets,
		policies:  policies,
	}

	svc := sandbox.New(sandbox.Config{
		Repo:        l.repo,
		Images:      fakeImages{r: r},
		Network:     l.net,
		Provider:    l.provider,
		Secrets:     secrets,
		Policies:    policies,
		Substrate:   l.substrate,
		PullTimeout: time.Minute,
	})

	return svc, l
}

// running is the record of a sandbox that is up, which is what stop and rm are given in most tests.
func running() models.Sandbox {
	return models.Sandbox{ID: "sandbox1", State: models.StateRunning, PID: 42}
}

// pausedSandbox is a sandbox that holds a snapshot, which is what resume, fork and clone are given.
func pausedSandbox() models.Sandbox {
	return models.Sandbox{ID: "sandbox1", Name: "web", State: models.StatePaused, Snapshot: "/snapshots/sandbox1",
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
