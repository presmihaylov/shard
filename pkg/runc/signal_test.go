package runc_test

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/presmihaylov/shard/pkg/runc"
)

func TestSignalRefusesAnUnreportedHostPID(t *testing.T) {
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := child.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		var exit *exec.ExitError
		if err := child.Wait(); err != nil && !errors.As(err, &exit) {
			t.Error(err)
		}
	})

	r, _ := fake(t, "", "", 0)
	if err := r.Signal(t.Context(), "amber-otter-1a2b", child.Process.Pid, "KILL"); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Signal of an unreported host PID = %v, want an ended exec", err)
	}
	if err := child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the unreported process received the signal: %v", err)
	}
}

func TestExecRefusesAReportWithoutALaunchPin(t *testing.T) {
	r, recorded := fake(t, "", "", 0)
	_, err := r.Exec(t.Context(), "sandbox-a", runc.ExecOptions{
		Argv: []string{"/bin/true"}, Bundle: bundle(t), Report: func(int) { t.Error("an unpinned exec reported a handle") },
	})
	if err == nil {
		t.Fatal("an exec reported a handle without a launch pin")
	}
	if _, err := os.Stat(recorded); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refusal ran the driver: %v", err)
	}
}
