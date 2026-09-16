//go:build integration

package sandbox_test

import (
	"context"
	"fmt"
	"io"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
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
	ticket, err := svc.CreateExec(context.Background(), "sandbox1", req)
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := svc.Attach(context.Background(), "sandbox1", ticket.ID, sandbox.Streams{Stdout: io.Discard})
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
