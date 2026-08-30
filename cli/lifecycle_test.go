package cli

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// fakeLifecycleRepo answers for one sandbox, so a test says what the record held before the verb ran.
type fakeLifecycleRepo struct {
	sandboxRepo

	r  *recorder
	sb models.Sandbox
	// left is what List answers with.
	left []models.Sandbox
	// unreadable is the error List answers beside the sandboxes it could read.
	unreadable error
	missing    bool
	deleted    bool
}

func (f *fakeLifecycleRepo) Get(id string) (models.Sandbox, error) {
	if err := f.r.record("repo.Get"); err != nil {
		return models.Sandbox{}, err
	}
	if f.missing {
		return models.Sandbox{}, fmt.Errorf("sandbox %s: %w", id, sandboxstate.ErrNotFound)
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

func (f *fakeLifecycleRepo) Update(_ string, mutate func(*models.Sandbox) error) error {
	if err := f.r.record("repo.Update"); err != nil {
		return err
	}

	return mutate(&f.sb)
}

func (f *fakeLifecycleRepo) Delete(string) error {
	if err := f.r.record("repo.Delete"); err != nil {
		return err
	}
	f.deleted = true

	return nil
}

type fakeLifecycleNet struct {
	sandboxNetwork

	r        *recorder
	released bool
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
	// logPath is the file logs reads, which a test writes into.
	logPath string
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

func (f *fakeLifecycleProvider) Start(context.Context, string) error {
	if err := f.r.record("provider.Start"); err != nil {
		return err
	}
	f.started = true
	f.status = models.Status{Exists: true, State: models.StateRunning, PID: 7}

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

	d := &deps{
		repoSvc:      &fakeLifecycleRepo{r: r, sb: sb},
		netSvc:       &fakeLifecycleNet{r: r},
		providerSvc:  &fakeLifecycleProvider{r: r, status: models.Status{Exists: true, State: sb.State}},
		substrateSvc: &fakeLifecycleSubstrate{r: r},
	}

	return App{
		Version: "test",
		Root:    t.TempDir(),
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
