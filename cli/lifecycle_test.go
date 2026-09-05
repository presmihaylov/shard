package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// recorder is the shared log of what the fakes were asked to do and in which order.
type recorder struct {
	fail  []string
	calls []string
	live  map[string]bool
}

func (r *recorder) record(name string) error {
	r.calls = append(r.calls, name)

	if slices.Contains(r.fail, name) {
		return fmt.Errorf("forced failure at %s", name)
	}

	return nil
}

// fakeImages hands a create through the daemon a rootfs without a registry.
type fakeImages struct {
	imageService
	r *recorder
}

func (f fakeImages) Hold(context.Context) (func() error, error) {
	if err := f.r.record("images.Hold"); err != nil {
		return nil, err
	}

	return func() error { return f.r.record("images.Release") }, nil
}

func (f fakeImages) Pull(context.Context, string) (image.Image, error) {
	if err := f.r.record("images.Pull"); err != nil {
		return image.Image{}, err
	}

	return image.Image{Reference: "alpine:3.20", RootFS: "/images/alpine"}, nil
}

// fakeLifecycleRepo answers for one sandbox, so a test says what the record held before the verb ran.
type fakeLifecycleRepo struct {
	r  *recorder
	sb models.Sandbox
	// left is what List answers with.
	left []models.Sandbox
	// unreadable is the error List answers beside the sandboxes it could read.
	unreadable error
	missing    bool
	deleted    bool
	// snapshotDir replaces the fixed path when a test needs the directory to exist on disk.
	snapshotDir string
	deletedID   string
	// created is the record Create was handed, which a fork fills in from the source.
	created models.Sandbox
}

func (f *fakeLifecycleRepo) Get(id string) (models.Sandbox, error) {
	if err := f.r.record("repo.Get"); err != nil {
		return models.Sandbox{}, err
	}
	if f.missing {
		return models.Sandbox{}, fmt.Errorf("sandbox %s: %w", id, sandboxstate.ErrNotFound)
	}
	if id == f.created.ID && id != "" {
		return f.created, nil
	}

	return f.sb, nil
}

func (f *fakeLifecycleRepo) Resolve(ref string) (string, error) {
	if f.sb.Name != "" && ref == f.sb.Name {
		return f.sb.ID, nil
	}

	return ref, nil
}

func (f *fakeLifecycleRepo) List() ([]models.Sandbox, error) {
	if err := f.r.record("repo.List"); err != nil {
		return nil, err
	}

	return f.left, f.unreadable
}

func (f *fakeLifecycleRepo) Update(id string, mutate func(*models.Sandbox) error) error {
	if err := f.r.record("repo.Update"); err != nil {
		return err
	}
	if id == f.created.ID && id != "" {
		return mutate(&f.created)
	}

	return mutate(&f.sb)
}

// Create hands out the id of the fork, and the record is kept beside the source's for the test to read.
func (f *fakeLifecycleRepo) Create(sb models.Sandbox) (models.Sandbox, error) {
	if err := f.r.record("repo.Create"); err != nil {
		return models.Sandbox{}, err
	}

	sb.ID = "sandbox2"
	f.created = sb

	return sb, nil
}

func (f *fakeLifecycleRepo) Dir(id string) (string, error) {
	if err := f.r.record("repo.Dir"); err != nil {
		return "", err
	}

	return "/state/" + id, nil
}

func (f *fakeLifecycleRepo) SnapshotDir(id string) (string, error) {
	if f.snapshotDir != "" {
		return f.snapshotDir, nil
	}

	return "/snapshots/" + id, nil
}

func (f *fakeLifecycleRepo) Delete(id string) error {
	if err := f.r.record("repo.Delete"); err != nil {
		return err
	}
	f.deleted = true
	f.deletedID = id

	return nil
}

type fakeLifecycleNet struct {
	r         *recorder
	allocated bool
	released  bool
	reapplied bool
}

func (f *fakeLifecycleNet) Allocate(_ context.Context, id string) (models.NetworkSpec, error) {
	if err := f.r.record("net.Allocate"); err != nil {
		return models.NetworkSpec{}, err
	}
	f.allocated = true

	return models.NetworkSpec{NetnsPath: "/run/netns/" + id, Address: netip.MustParsePrefix("10.0.0.2/24"), HostInterface: "veth-" + id}, nil
}

func (f *fakeLifecycleNet) Reapply(context.Context, string) error {
	if err := f.r.record("net.Reapply"); err != nil {
		return err
	}
	f.reapplied = true

	return nil
}

func (f *fakeLifecycleNet) ReapplyAll(context.Context) error {
	if err := f.r.record("net.ReapplyAll"); err != nil {
		return err
	}
	f.reapplied = true

	return nil
}

func (f *fakeLifecycleNet) Release(context.Context, string) error {
	if err := f.r.record("net.Release"); err != nil {
		return err
	}
	f.released = true

	return nil
}

type fakeLifecycleProvider struct {
	models.Provider

	r      *recorder
	status models.Status
	exit   models.ExitStatus
	// waitErr is what a sandbox the stop had to kill answers with: it recorded no exit status.
	waitErr error

	grace   time.Duration
	started bool
	stopped bool
	removed bool
	// snapshot is the directory the pause was told to write, or the resume or fork was told to read.
	snapshot string
	forked   models.SandboxSpec
	created  models.SandboxSpec
	// clonedFrom and cloned are the source id and the spec Clone was handed.
	clonedFrom string
	cloned     models.SandboxSpec
	// noPause, noResume and noFork take a verb out of what the provider claims.
	noPause, noResume, noFork bool
	// logPath is the file logs reads, which a test writes into.
	logPath string
}

func (f *fakeLifecycleProvider) Name() string { return "fake" }

// Capabilities claims every optional verb unless a test takes one away.
func (f *fakeLifecycleProvider) Capabilities() models.Capabilities {
	return models.Capabilities{Pause: !f.noPause, Resume: !f.noResume, Fork: !f.noFork}
}

func (f *fakeLifecycleProvider) LogPath(string) (string, error) {
	if err := f.r.record("provider.LogPath"); err != nil {
		return "", err
	}

	return f.logPath, nil
}

func (f *fakeLifecycleProvider) Status(context.Context, string) (models.Status, error) {
	if err := f.r.record("provider.Status"); err != nil {
		return models.Status{}, err
	}

	return f.status, nil
}

// Create records the spec, so a test says what a create through the daemon handed the substrate.
func (f *fakeLifecycleProvider) Create(_ context.Context, spec models.SandboxSpec) error {
	f.created = spec

	return f.r.record("provider.Create")
}

func (f *fakeLifecycleProvider) Start(context.Context, string) error {
	if err := f.r.record("provider.Start"); err != nil {
		return err
	}
	f.started = true
	f.status = models.Status{Exists: true, State: models.StateRunning, PID: 7}

	return nil
}

func (f *fakeLifecycleProvider) Pause(_ context.Context, _ string, dir string) error {
	if err := f.r.record("provider.Pause"); err != nil {
		return err
	}
	f.snapshot = dir
	// runsc holds nothing of a paused sandbox, so its status reads as one that never existed.
	f.status = models.Status{}

	return nil
}

func (f *fakeLifecycleProvider) Resume(_ context.Context, _ string, dir string) error {
	if err := f.r.record("provider.Resume"); err != nil {
		return err
	}
	f.snapshot = dir
	f.status = models.Status{Exists: true, State: models.StateRunning, PID: 7}

	return nil
}

// Fork records the spec it was handed, so a test says what identity the new sandbox got.
func (f *fakeLifecycleProvider) Fork(_ context.Context, dir string, spec models.SandboxSpec) error {
	if err := f.r.record("provider.Fork"); err != nil {
		return err
	}
	f.snapshot = dir
	f.forked = spec
	f.status = models.Status{Exists: true, State: models.StateRunning, PID: 9}

	return nil
}

// Clone records the source and the spec, so a test says what the new sandbox was started over.
func (f *fakeLifecycleProvider) Clone(_ context.Context, sourceID string, spec models.SandboxSpec) error {
	if err := f.r.record("provider.Clone"); err != nil {
		return err
	}
	f.clonedFrom = sourceID
	f.cloned = spec
	f.status = models.Status{Exists: true, State: models.StateRunning, PID: 11}

	return nil
}

func (f *fakeLifecycleProvider) Stop(_ context.Context, _ string, grace time.Duration) error {
	if err := f.r.record("provider.Stop"); err != nil {
		return err
	}
	f.stopped, f.grace = true, grace
	f.status = models.Status{Exists: true, State: models.StateStopped}

	return nil
}

func (f *fakeLifecycleProvider) Remove(context.Context, string) error {
	if err := f.r.record("provider.Remove"); err != nil {
		return err
	}
	f.removed = true

	return nil
}

func (f *fakeLifecycleProvider) Wait(context.Context, string) (models.ExitStatus, error) {
	if err := f.r.record("provider.Wait"); err != nil {
		return models.ExitStatus{}, err
	}
	if f.waitErr != nil {
		return models.ExitStatus{}, f.waitErr
	}

	return f.exit, nil
}

// fakeLifecycleSubstrate stands in for the runsc root, which off Linux has no mount to give back.
type fakeLifecycleSubstrate struct {
	r       *recorder
	dropped bool
}

func (f *fakeLifecycleSubstrate) DropNullNetns() error {
	if err := f.r.record("substrate.DropNullNetns"); err != nil {
		return err
	}
	f.dropped = true

	return nil
}

// newLifecycleApp wires stop and rm onto fakes, so the order and the refusals are testable off Linux.
func newLifecycleApp(t *testing.T, out *bytes.Buffer, r *recorder, sb models.Sandbox) (App, *deps) {
	t.Helper()

	r.live = map[string]bool{}
	// A short root, so the verbs that speak to a daemon can put a socket under it.
	root := shortRoot(t)

	d := &deps{
		app:          App{Root: root},
		repoSvc:      &fakeLifecycleRepo{r: r, sb: sb},
		netSvc:       &fakeLifecycleNet{r: r},
		providerSvc:  &fakeLifecycleProvider{r: r, status: models.Status{Exists: true, State: sb.State}},
		substrateSvc: &fakeLifecycleSubstrate{r: r},
	}

	return App{
		Version: "test",
		Root:    root,
		Out:     out,
		Err:     out,
		Timeout: time.Minute,
		newDeps: func(App) *deps { return d },
	}, d
}

// running is the record of a sandbox that is up, which is what stop and rm are given in most tests.
func running() models.Sandbox {
	return models.Sandbox{ID: "sandbox1", State: models.StateRunning, PID: 42}
}

func stopped() models.Sandbox {
	return models.Sandbox{ID: "sandbox1", Name: "web", State: models.StateStopped, ExitStatus: &models.ExitStatus{Code: 3}}
}

func (f *fakeLifecycleRepo) Hold(_ context.Context, id string) (func() error, error) {
	if err := f.r.record("repo.Hold"); err != nil {
		return nil, err
	}
	if f.missing {
		return nil, fmt.Errorf("sandbox %s: %w", id, sandboxstate.ErrNotFound)
	}

	return func() error { return nil }, nil
}

// HoldShared is recorded apart from Hold, so a test says which one a verb took.
func (f *fakeLifecycleRepo) HoldShared(_ context.Context, id string) (func() error, error) {
	if err := f.r.record("repo.HoldShared"); err != nil {
		return nil, err
	}
	if f.missing {
		return nil, fmt.Errorf("sandbox %s: %w", id, sandboxstate.ErrNotFound)
	}

	return func() error { return nil }, nil
}
