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
	r       *recorder
	sb      models.Sandbox
	missing bool
	deleted bool
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
	r      *recorder
	status models.Status
	exit   models.ExitStatus
	// waitErr is what a sandbox the stop had to kill answers with: it recorded no exit status.
	waitErr error

	grace   time.Duration
	stopped bool
	removed bool
}

func (f *fakeLifecycleProvider) Status(context.Context, string) (models.Status, error) {
	if err := f.r.record("provider.Status"); err != nil {
		return models.Status{}, err
	}

	return f.status, nil
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

// newLifecycleApp wires stop and rm onto fakes, so the order and the refusals are testable off Linux.
func newLifecycleApp(t *testing.T, out *bytes.Buffer, r *recorder, sb models.Sandbox) (App, lifecycleDeps) {
	t.Helper()

	r.live = map[string]bool{}

	deps := lifecycleDeps{
		repo:     &fakeLifecycleRepo{r: r, sb: sb},
		net:      &fakeLifecycleNet{r: r},
		provider: &fakeLifecycleProvider{r: r, status: models.Status{Exists: true, State: sb.State}},
	}

	return App{
		Version:          "test",
		Root:             t.TempDir(),
		Out:              out,
		Err:              out,
		Timeout:          time.Minute,
		newLifecycleDeps: func(App) (lifecycleDeps, error) { return deps, nil },
	}, deps
}

// running is the record of a sandbox that is up, which is what stop and rm are given in most tests.
func running() models.Sandbox {
	return models.Sandbox{ID: "sandbox1", State: models.StateRunning, PID: 42}
}
