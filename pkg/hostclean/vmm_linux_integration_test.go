//go:build integration && linux

package hostclean

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// startVMM runs a stand-in vmm that names sock, and blocks on a read so no exec of the shell drops the argument.
func startVMM(t *testing.T, sock string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", "read x", "sh", apiSockFlag, sock)
	// Wait closes the pipe, so the read holds until the test reaps the shell.
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState != nil {
			return
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Error(err)
		}
		reap(t, cmd)
	})

	// Start returns once the old mm is gone, which can be before the kernel fills in the new command line.
	cmdline := filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "cmdline")
	deadline := time.Now().Add(killGrace)
	for {
		argv, err := os.ReadFile(cmdline)
		if err != nil {
			t.Fatal(err)
		}
		if apiSocket(strings.Split(string(argv), "\x00")) == sock {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stand-in vmm names %q, want %s", argv, sock)
		}
		time.Sleep(pollInterval)
	}
}

// reap waits for a stand-in vmm that a SIGKILL ended.
func reap(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	var exit *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Errorf("wait = %v, want a SIGKILL", err)
	}
}

// running is whether pid is alive and not a zombie.
func running(t *testing.T, pid int) bool {
	t.Helper()
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if gone(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))

	return fields[0] != "Z"
}

// The kill returns once the vmm exits, before its parent reaps it, so a zombie holds no sweep up.
func TestAPinnedVMMDiesThroughItsPin(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "fc.sock")
	cmd := startVMM(t, sock)

	vmm, ours, err := pinVMM(cmd.Process.Pid, sock)
	if err != nil || !ours || vmm.pin == nil {
		t.Fatalf("pinVMM = %v, %v, pin %v; want ours with a pin", ours, err, vmm.pin)
	}
	if err := vmm.remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if running(t, cmd.Process.Pid) {
		t.Fatal("the vmm still runs after remove")
	}
	reap(t, cmd)
}

// Another root's socket shares the prefix but not the argument, so the pin lets go and the process runs on.
func TestAVMMOfAnotherSocketIsNotOurs(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "fc.sock")
	cmd := startVMM(t, sock+"-other")

	vmm, ours, err := pinVMM(cmd.Process.Pid, sock)
	if err != nil || ours || vmm.pin != nil {
		t.Fatalf("pinVMM = %v, %v, pin %v; want not ours", ours, err, vmm.pin)
	}
	if !running(t, cmd.Process.Pid) {
		t.Error("a vmm that is not ours died")
	}
}

// A vmm reaped between the pin and the kill leaves the pidfd, not its pid, so nothing else takes the signal.
func TestAVMMReapedAfterItsPinIsNothingToKill(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "fc.sock")
	cmd := startVMM(t, sock)

	vmm, ours, err := pinVMM(cmd.Process.Pid, sock)
	if err != nil || !ours {
		t.Fatalf("pinVMM = %v, %v; want ours", ours, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	reap(t, cmd)
	if err := vmm.remove(); err != nil {
		t.Errorf("remove of a reaped vmm: %v", err)
	}
}

// A pid the pin refuses is never signalled, and the sweep names it rather than pass over it.
func TestAVMMThatCannotBePinnedIsNamedAndSpared(t *testing.T) {
	orig := openPidfd
	t.Cleanup(func() { openPidfd = orig })
	openPidfd = func(int, int) (int, error) { return -1, unix.EPERM }

	sock := filepath.Join(t.TempDir(), "fc.sock")
	cmd := startVMM(t, sock)
	pid := cmd.Process.Pid

	vmm, ours, err := pinVMM(pid, sock)
	if err != nil || !ours || vmm.pin != nil {
		t.Fatalf("pinVMM = %v, %v, pin %v; want a leftover with no pin", ours, err, vmm.pin)
	}
	err = removeEach([]Leftover{vmm})
	if err == nil || !strings.Contains(err.Error(), strconv.Itoa(pid)) || !strings.Contains(err.Error(), sock) {
		t.Errorf("removeEach = %v, want an error naming pid %d and %s", err, pid, sock)
	}
	if !running(t, pid) {
		t.Error("a vmm the pin refused was signalled")
	}
}
