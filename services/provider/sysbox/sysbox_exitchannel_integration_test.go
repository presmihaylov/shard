//go:build integration

package sysbox_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestARestartedDaemonStopsASandboxWhoseFdZeroIsAWriteOnlySysfsFile is SHARD-614: kernfs refuses root a read open of a 0200 file, which failed the stop.
func TestARestartedDaemonStopsASandboxWhoseFdZeroIsAWriteOnlySysfsFile(t *testing.T) {
	h := newHarness(t)

	found, err := filepath.Glob("/sys/bus/*/uevent")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Skip("no write-only sysfs file at /sys/bus/*/uevent on this host")
	}

	spec := h.newSpec(t)
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	// Only the /proc magic link is faked, since a test cannot dup2 into PID 1; the pid, the cgroup and the kernfs file are real.
	procRoot := t.TempDir()
	fdDir := filepath.Join(procRoot, strconv.Itoa(status.PID), "fd")
	if err := os.MkdirAll(fdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(found[0], filepath.Join(fdDir, "0")); err != nil {
		t.Fatal(err)
	}

	restarted, err := h.open()
	if err != nil {
		t.Fatalf("open the provider again: %v", err)
	}
	restarted.SetProcRoot(procRoot)

	if err := restarted.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop with %s on fd 0 returned %v, want the sandbox stopped", found[0], err)
	}
	status, err = restarted.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("Status after Stop: %v", err)
	}
	if status.Alive() {
		t.Errorf("Status after Stop is %+v, want the sandbox not alive", status)
	}
}
