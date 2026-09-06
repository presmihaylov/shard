package daemon

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/store"
)

// fakeTask counts its runs and answers each one from a script; past the script it blocks until ctx ends.
type fakeTask struct {
	runs   atomic.Int32
	script []error
}

func (t *fakeTask) Name() string { return "fake" }

func (t *fakeTask) Run(ctx context.Context) error {
	n := int(t.runs.Add(1))
	if n <= len(t.script) {
		return t.script[n-1]
	}
	<-ctx.Done()

	return ctx.Err()
}

// fast makes the backoff too short to slow a test down.
func fast(d *Daemon) *Daemon {
	d.minBackoff = time.Millisecond
	d.maxBackoff = 4 * time.Millisecond
	d.healthyAfter = time.Hour

	return d
}

func TestRunRefusesASecondDaemon(t *testing.T) {
	root := t.TempDir()
	first := fast(New(root, io.Discard))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- first.Run(ctx) }()

	waitHeld(t, root)

	if err := New(root, io.Discard).Run(t.Context()); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("a second daemon got %v, want a refusal", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("the first daemon ended with %v", err)
	}
}

func TestSuperviseRestartsAFailingTaskUntilItIsDone(t *testing.T) {
	task := &fakeTask{script: []error{errors.New("one"), errors.New("two"), nil}}
	d := fast(New(t.TempDir(), io.Discard))

	d.supervise(t.Context(), task)

	if got := task.runs.Load(); got != 3 {
		t.Errorf("the task ran %d times, want 3", got)
	}
}

func TestSuperviseStopsWhenTheContextEnds(t *testing.T) {
	task := &fakeTask{}
	d := fast(New(t.TempDir(), io.Discard))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		d.supervise(ctx, task)
		close(done)
	}()

	for task.runs.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervise did not stop with the context")
	}
}

func TestSuperviseContainsAPanic(t *testing.T) {
	task := &panicTask{}
	d := fast(New(t.TempDir(), io.Discard))

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		for task.runs.Load() < 2 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	// A panic that escapes fails the test on its own; reaching here twice proves it was contained.
	d.supervise(ctx, task)
}

type panicTask struct {
	runs atomic.Int32
}

func (t *panicTask) Name() string { return "panics" }

func (t *panicTask) Run(context.Context) error {
	t.runs.Add(1)
	panic("boom")
}

func TestRunEndsCleanlyWithZeroTasks(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := New(t.TempDir(), io.Discard).Run(ctx); err != nil {
		t.Fatalf("Run = %v", err)
	}
}

// waitHeld waits until the daemon under root holds the singleton lock.
func waitHeld(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		lock, err := store.TryAcquire(filepath.Join(root, LockFile), 0o600)
		if err != nil {
			t.Fatalf("TryAcquire: %v", err)
		}
		if lock == nil {
			return
		}
		if err := lock.Release(); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon never took the lock")
		}
		time.Sleep(time.Millisecond)
	}
}

// A fresh host has no root yet; the lock's acquire creates it, so serve needs no verb to run first.
func TestRunCreatesAFreshRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does", "not", "exist")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := New(root, io.Discard).Run(ctx); err != nil {
		t.Fatalf("serve on a fresh root: %v", err)
	}
}

// fakeReconciler stands in for the check of the records, and says whether a task had already started.
type fakeReconciler struct {
	err     error
	ran     atomic.Bool
	reports []string
	// task is the one the daemon supervises, and tasks is how many runs it had when this was called.
	task  *fakeTask
	tasks int32
}

func (f *fakeReconciler) Reconcile(_ context.Context, report func(string)) error {
	if f.task != nil {
		f.tasks = f.task.runs.Load()
	}
	f.ran.Store(true)
	for _, line := range f.reports {
		report(line)
	}

	return f.err
}

func TestRunReconcilesBeforeItStartsATask(t *testing.T) {
	task := &fakeTask{}
	rec := &fakeReconciler{task: task}
	d := fast(New(t.TempDir(), io.Discard, task)).WithReconciler(rec)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	for task.runs.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()

	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	if !rec.ran.Load() {
		t.Fatal("the daemon started a task and never checked the records")
	}
	if rec.tasks != 0 {
		t.Errorf("%d tasks had started when the records were checked, want none", rec.tasks)
	}
}

func TestRunRefusesToServeOverRecordsItCouldNotCheck(t *testing.T) {
	rec := &fakeReconciler{err: errors.New("runsc is not on this host")}
	task := &fakeTask{}

	err := fast(New(t.TempDir(), io.Discard, task)).WithReconciler(rec).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "runsc is not on this host") {
		t.Fatalf("Run = %v, want the reconcile's own refusal", err)
	}
	if task.runs.Load() != 0 {
		t.Errorf("the api task ran %d times over records the daemon could not check", task.runs.Load())
	}
}

func TestRunLogsWhatTheReconcileCorrected(t *testing.T) {
	rec := &fakeReconciler{reports: []string{"sandbox1 is stopped"}}
	var out strings.Builder

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := fast(New(t.TempDir(), &out)).WithReconciler(rec).Run(ctx); err != nil {
		t.Fatalf("Run = %v", err)
	}

	if !strings.Contains(out.String(), "sandbox1 is stopped") {
		t.Errorf("the daemon logged %q, want the line the reconcile reported", out.String())
	}
}
