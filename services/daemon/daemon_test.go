package daemon

import (
	"context"
	"errors"
	"io"
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
	first := fast(New(root, io.Discard))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- first.Run(ctx) }()

	waitAlive(t, root)

	if err := New(root, io.Discard).Run(t.Context()); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("a second daemon got %v, want a refusal", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("the first daemon ended with %v", err)
	}
}

func TestAliveSeesTheHolderAndItsAbsence(t *testing.T) {
	root := t.TempDir()

	alive, err := Alive(root)
	if err != nil || alive {
		t.Fatalf("Alive on an idle root = %v, %v", alive, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- fast(New(root, io.Discard)).Run(ctx) }()

	waitAlive(t, root)

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("the daemon ended with %v", err)
	}

	alive, err = Alive(root)
	if err != nil || alive {
		t.Errorf("Alive after the daemon ended = %v, %v", alive, err)
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

func waitAlive(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		alive, err := Alive(root)
		if err != nil {
			t.Fatalf("Alive: %v", err)
		}
		if alive {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon never took the lock")
		}
		time.Sleep(time.Millisecond)
	}
}
