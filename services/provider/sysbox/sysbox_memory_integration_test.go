//go:build integration

package sysbox_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/services/bundle"
)

// boundMiB is the bound the sandbox gets. Sysbox has no sentry, so the whole bound is the guest's.
const boundMiB = 64

// TestABoundSandboxPinsSwapAndGroupsItsOOMKill is the SHARD-191 acceptance. sysbox-runc sets
// memory.max from config.json but neither knob, so a bound took one guest process and the sandbox
// lived, and restart_on_oom never fired. gvisor's provider sets the same pair.
func TestABoundSandboxPinsSwapAndGroupsItsOOMKill(t *testing.T) {
	h := newHarness(t)

	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	spec.Resources = models.Resources{MemoryMiB: boundMiB}

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	dir := filepath.Join(cgroup.Root, bundle.CgroupsPath(spec.ID))

	// Both files are read raw, so the assertion does not lean on the driver that wrote them.
	if got := strings.TrimSpace(readFile(t, filepath.Join(dir, "memory.swap.max"))); got != "0" {
		t.Errorf("memory.swap.max is %s, want 0", got)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(dir, "memory.oom.group"))); got != "1" {
		t.Errorf("memory.oom.group is %s, want 1", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(b)
}
