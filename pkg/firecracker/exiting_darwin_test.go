//go:build darwin

package firecracker

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestExitingSaysALiveProcessIsNotAndAZombieOrAReapedOneIs(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	wait := sync.OnceValue(cmd.Wait)
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		var exit *exec.ExitError
		if err := wait(); err != nil && !errors.As(err, &exit) {
			t.Error(err)
		}
	})

	if leaving, err := exiting(os.Getpid()); err != nil || leaving {
		t.Fatalf("exiting of this process = %v, %v, want false", leaving, err)
	}
	if leaving, err := exiting(pid); err != nil || leaving {
		t.Fatalf("exiting of a live child = %v, %v, want false", leaving, err)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !zombied(t, pid) {
		if time.Now().After(deadline) {
			t.Fatal("the child is no zombie 5s after its kill")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if leaving, err := exiting(pid); err != nil || !leaving {
		t.Fatalf("exiting of a zombie = %v, %v, want true", leaving, err)
	}

	var exit *exec.ExitError
	if err := wait(); !errors.As(err, &exit) {
		t.Fatalf("reap the killed child: %v", err)
	}
	if leaving, err := exiting(pid); err != nil || !leaving {
		t.Fatalf("exiting of a reaped child = %v, %v, want true", leaving, err)
	}
}

func zombied(t *testing.T, pid int) bool {
	t.Helper()
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		t.Fatal(err)
	}

	return len(procs) == 1 && procs[0].Proc.P_stat == zombie
}
