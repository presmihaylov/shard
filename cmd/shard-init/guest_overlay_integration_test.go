//go:build linux && integration

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestRefuseReadOnlyMount proves the boot refuses a read-only overlay root with a named error, and passes a writable mount (SHARD-344).
func TestRefuseReadOnlyMount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("mounting an overlay needs root")
	}

	lowerA := overlayDir(t, "lowerA")
	lowerB := overlayDir(t, "lowerB")
	merged := overlayDir(t, "merged")
	// Two lowers with no upper is a read-only overlay, the state a corrupt work dir forces; its statfs reports ST_RDONLY.
	if err := unix.Mount("overlay", merged, "overlay", 0, "lowerdir="+lowerA+":"+lowerB); err != nil {
		t.Fatalf("mount a read-only overlay: %v", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(merged, 0) })

	err := refuseReadOnlyMount(merged, "/dev/vdb")
	if err == nil || !strings.Contains(err.Error(), "read-only") || !strings.Contains(err.Error(), "/dev/vdb") {
		t.Fatalf("refuse a read-only overlay = %v, want it refused and the overlay named", err)
	}

	if err := refuseReadOnlyMount(overlayDir(t, "rw"), "/dev/vdb"); err != nil {
		t.Fatalf("refuse a writable mount = %v, want nil", err)
	}
}

func overlayDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	return dir
}
