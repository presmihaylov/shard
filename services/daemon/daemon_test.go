package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	held := &fakeTask{}
	first := fast(New(root, io.Discard, held))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- first.Run(ctx) }()

	waitHeld(t, held)

	if err := New(root, io.Discard).Run(t.Context()); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("a second daemon got %v, want a refusal", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("the first daemon ended with %v", err)
	}
}

func TestRunNamesItsPidUntilItEnds(t *testing.T) {
	root := t.TempDir()
	held := &fakeTask{}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- fast(New(root, io.Discard, held)).Run(ctx) }()

	waitHeld(t, held)

	pid := filepath.Join(root, PIDFile)
	got, err := os.ReadFile(pid)
	if err != nil {
		t.Fatalf("read the pid file: %v", err)
	}
	if want := strconv.Itoa(os.Getpid()) + "\n"; string(got) != want {
		t.Errorf("the pid file holds %q, want %q", got, want)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("the daemon ended with %v", err)
	}
	if _, err := os.Stat(pid); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the pid file outlived the daemon: %v", err)
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

func TestTaskStatesTrackRunningBackoffAndDone(t *testing.T) {
	ts := newTaskStates([]Task{&fakeTask{}})

	if got := ts.snapshot(); len(got) != 1 || got[0].Name != "fake" || got[0].State != TaskRunning {
		t.Fatalf("a fresh registry = %+v, want one running task", got)
	}

	ts.backoff("fake", errors.New("one"))
	ts.backoff("fake", errors.New("two"))
	if got := ts.snapshot()[0]; got.State != TaskBackoff || got.Restarts != 2 || got.LastError != "two" {
		t.Errorf("after two failures = %+v, want backoff/2/two", got)
	}

	ts.running("fake")
	if got := ts.snapshot()[0]; got.State != TaskRunning || got.Restarts != 2 {
		t.Errorf("after a restart = %+v, want running with the count kept", got)
	}

	ts.done("fake")
	if got := ts.snapshot()[0]; got.State != TaskDone {
		t.Errorf("after it is done = %+v, want done", got)
	}
}

func TestSuperviseRecordsEachTaskState(t *testing.T) {
	task := &fakeTask{script: []error{errors.New("one"), errors.New("two"), nil}}
	d := fast(New(t.TempDir(), io.Discard, task))

	d.supervise(t.Context(), task)

	got := d.states.snapshot()
	if len(got) != 1 {
		t.Fatalf("snapshot = %+v, want one task", got)
	}
	if s := got[0]; s.Name != "fake" || s.State != TaskDone || s.Restarts != 2 || s.LastError != "two" {
		t.Errorf("after supervise = %+v, want fake/done/2/two", s)
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

// waitHeld waits until the daemon holds the singleton lock, which its first task starting proves: Run
// takes the lock before it supervises anything. The test must never take the lock to find out, because
// takeLock is single-shot and would lose to the probe and blame the daemon for it.
func waitHeld(t *testing.T, started *fakeTask) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for started.runs.Load() == 0 {
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
