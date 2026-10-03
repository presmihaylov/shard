//go:build integration

package gvisor_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/provider/gvisor"
)

// SHARD-440: the teardown kills a sandbox's own processes by cgroup through a pinned pidfd, so Remove spares a sentry or gofer pid the kernel reused after the sandbox died.
func TestRemoveSparesHostProcessesThatReusedTheStoredPids(t *testing.T) {
	requireNsLastPid(t)
	h := newHarness(t)
	spec := h.start(t, "/bin/sh", "-c", "sleep 3600")

	sentry := h.sentryPID(t, spec.ID)
	gofer := h.goferPID(t, spec.ID)
	if sentry <= 0 || gofer <= 0 {
		t.Fatalf("sandbox %s has no sentry and gofer pid: sentry=%d gofer=%d", spec.ID, sentry, gofer)
	}

	// Kill the sandbox's own sentry and gofer so their pids free up, the way a real sandbox death frees them.
	killAndReap(t, sentry)
	killAndReap(t, gofer)

	// An unrelated host process grabs each freed pid before Remove runs.
	first := reuse(t, sentry)
	second := reuse(t, gofer)
	defer release(first)
	defer release(second)

	if err := h.provider.Remove(t.Context(), spec.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if !alive(t, first.Process.Pid) {
		t.Errorf("Remove killed the host process that reused the sentry pid %d", sentry)
	}
	if !alive(t, second.Process.Pid) {
		t.Errorf("Remove killed the host process that reused the gofer pid %d", gofer)
	}

	assertFreed(t, h, spec.ID)
}

func (h *harness) sentryPID(t *testing.T, id string) int {
	t.Helper()

	out, err := h.runsc(t, "state", id).Output()
	if err != nil {
		t.Fatalf("runsc state %s: %v", id, err)
	}

	var state struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(out, &state); err != nil {
		t.Fatalf("decode the state of %s: %v", id, err)
	}

	return state.PID
}

func (h *harness) goferPID(t *testing.T, id string) int {
	t.Helper()

	states, err := filepath.Glob(filepath.Join(h.runscRoot, "*:"+id+".state"))
	if err != nil || len(states) != 1 {
		t.Fatalf("find the runsc state of %s: %v %v", id, states, err)
	}

	raw, err := os.ReadFile(states[0])
	if err != nil {
		t.Fatalf("read the runsc state of %s: %v", id, err)
	}

	var state struct {
		GoferPID int `json:"goferPid"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode the runsc state of %s: %v", id, err)
	}

	return state.GoferPID
}

func requireNsLastPid(t *testing.T) {
	t.Helper()

	if _, err := os.Stat("/proc/sys/kernel/ns_last_pid"); err != nil {
		t.Skipf("no ns_last_pid on this host, cannot force a pid reuse: %v", err)
	}
}

// killAndReap SIGKILLs a pid that is not our child and waits for init to reap it, so the pid is free to reuse.
func killAndReap(t *testing.T, pid int) {
	t.Helper()

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		t.Fatalf("kill pid %d: %v", pid, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d did not exit after SIGKILL", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// reuse makes a host process take a specific freed pid, by rewinding ns_last_pid and forking until the fork lands on it.
func reuse(t *testing.T, pid int) *exec.Cmd {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := os.WriteFile("/proc/sys/kernel/ns_last_pid", []byte(strconv.Itoa(pid-1)), 0o644); err != nil {
			t.Fatalf("set ns_last_pid for %d: %v", pid, err)
		}

		cmd := exec.Command("sleep", "600")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start a reuser for pid %d: %v", pid, err)
		}
		if cmd.Process.Pid == pid {
			return cmd
		}

		release(cmd)
	}

	t.Fatalf("could not make a host process reuse pid %d", pid)

	return nil
}

func release(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// alive reports whether a pid is a running process, not a corpse a SIGKILL left behind for us to reap.
func alive(t *testing.T, pid int) bool {
	t.Helper()

	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}

	return !gvisor.ZombieStat(string(raw))
}
