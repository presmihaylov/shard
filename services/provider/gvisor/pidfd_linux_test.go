//go:build linux

package gvisor_test

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"

	"github.com/presmihaylov/shard/services/provider/gvisor"
)

// The pinned kill reaches the process only when the check still holds, and a reaped one answers ESRCH.
func TestPidfdKillKillsOnlyWhatStillHolds(t *testing.T) {
	spared := exec.Command("sleep", "60")
	if err := spared.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		if err := spared.Process.Kill(); err != nil {
			t.Errorf("end the spared sleep: %v", err)
		}
		if err := spared.Wait(); err == nil {
			t.Error("the spared sleep exited cleanly, want the cleanup kill")
		}
	})
	if err := gvisor.PidfdKill(spared.Process.Pid, func() (bool, error) { return false, nil }); err != nil {
		t.Fatalf("PidfdKill on a check that fails: %v", err)
	}
	if err := spared.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("PidfdKill killed a process whose check failed: %v", err)
	}

	killed := exec.Command("sleep", "60")
	if err := killed.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	if err := gvisor.PidfdKill(killed.Process.Pid, func() (bool, error) { return true, nil }); err != nil {
		t.Fatalf("PidfdKill: %v", err)
	}
	if err := killed.Wait(); err == nil || killed.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("the sleep ended with %v, want SIGKILL", err)
	}

	// A check that fails the test, because a pid reused this fast must still never be killed.
	reaped := func() (bool, error) { return false, errors.New("the check ran on a reaped pid") }
	if err := gvisor.PidfdKill(killed.Process.Pid, reaped); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("PidfdKill on a reaped process returned %v, want ESRCH", err)
	}
}
