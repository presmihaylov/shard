//go:build integration

package sandbox_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
	"github.com/presmihaylov/shard/services/sandbox"
)

// execReturnBudget bounds an exec that must come back, so a wait that never ends fails the test.
const execReturnBudget = 30 * time.Second

// replicaHolder is a command that left something behind: it keeps the terminal it was given open.
type replicaHolder struct {
	models.Provider

	t *testing.T
}

func (h *replicaHolder) Name() string { return "fake" }

func (h *replicaHolder) Status(context.Context, string) (models.Status, error) {
	return models.Status{Exists: true, State: models.StateRunning}, nil
}

func (h *replicaHolder) Exec(_ context.Context, _ string, spec models.ExecSpec) (models.ExitStatus, error) {
	kept, err := syscall.Dup(int(spec.Stdin.Fd()))
	if err != nil {
		return models.ExitStatus{}, fmt.Errorf("keep a copy of the replica: %w", err)
	}
	h.t.Cleanup(func() {
		if err := syscall.Close(kept); err != nil {
			h.t.Logf("drop the kept replica: %v", err)
		}
	})

	return models.ExitStatus{}, nil
}

// A command can leave a process holding the terminal it was given, and then nothing ever closes the
// guest side. The exit status is already in hand, so the daemon must let go rather than wait for it.
func TestExecOnATerminalLetsGoOfOutputNothingWillEnd(t *testing.T) {
	r := &recorder{live: map[string]bool{}}
	svc := sandbox.New(sandbox.Config{Repo: &fakeRepo{r: r, sb: running()}, Provider: &replicaHolder{t: t}})

	req := sandbox.ExecRequest{Command: []string{"/bin/true"}, TTY: true}
	exec, err := svc.CreateExec(context.Background(), "sandbox1", req)
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := svc.Attach(context.Background(), "sandbox1", exec.ID, sandbox.Streams{Stdout: io.Discard})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
	case <-time.After(execReturnBudget):
		t.Fatalf("the exec did not return within %s, and the command it ran is long gone", execReturnBudget)
	}
}

// terminalHolder keeps the replica it was given and hands it out as a file, so a test can resize the exec
// while the command still runs and read the new window from the guest side. The test closes that file.
type terminalHolder struct {
	models.Provider

	replica chan *os.File
	release chan struct{}
}

func (h *terminalHolder) Name() string { return "fake" }

func (h *terminalHolder) Status(context.Context, string) (models.Status, error) {
	return models.Status{Exists: true, State: models.StateRunning}, nil
}

func (h *terminalHolder) Exec(_ context.Context, _ string, spec models.ExecSpec) (models.ExitStatus, error) {
	kept, err := syscall.Dup(int(spec.Stdin.Fd()))
	if err != nil {
		return models.ExitStatus{}, fmt.Errorf("keep a copy of the replica: %w", err)
	}
	// The os.File is the one owner of the dup; the test closes it, so nothing raw-closes the number here.
	h.replica <- os.NewFile(uintptr(kept), "replica")
	<-h.release

	return models.ExitStatus{}, nil
}

// A resize reaches a command that is still running: shard sizes the master, and the guest side of the
// terminal reads the new window at once.
func TestExecResizeReachesARunningTerminal(t *testing.T) {
	r := &recorder{live: map[string]bool{}}
	holder := &terminalHolder{replica: make(chan *os.File, 1), release: make(chan struct{})}
	svc := sandbox.New(sandbox.Config{Repo: &fakeRepo{r: r, sb: running()}, Provider: holder})

	req := sandbox.ExecRequest{Command: []string{"/bin/true"}, TTY: true}
	exec, err := svc.CreateExec(context.Background(), "sandbox1", req)
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := svc.Attach(context.Background(), "sandbox1", exec.ID, sandbox.Streams{Stdout: io.Discard})
		done <- err
	}()

	replica := <-holder.replica

	want := pty.Size{Rows: 40, Cols: 120}
	if err := svc.ResizeExec(context.Background(), "sandbox1", exec.ID, sandbox.TerminalSize{Rows: want.Rows, Cols: want.Cols}); err != nil {
		t.Fatalf("ResizeExec: %v", err)
	}

	got, err := pty.SizeOf(replica)
	if err != nil {
		t.Fatalf("read the window of the running terminal: %v", err)
	}
	if got != want {
		t.Errorf("the running terminal reads %v, want %v after the resize", got, want)
	}

	// One owner: closing the file here clears its finalizer, so nothing closes the number a second time.
	if err := replica.Close(); err != nil {
		t.Fatalf("drop the kept replica: %v", err)
	}
	close(holder.release)
	if err := <-done; err != nil {
		t.Fatalf("Attach: %v", err)
	}
}
