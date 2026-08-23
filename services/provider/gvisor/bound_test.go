package gvisor_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/provider/gvisor"
)

// fakeCgroup stands in for the cgroup runsc makes: memory.max holds what runsc applied, and
// memory.high holds what a fresh cgroup holds, which is no throttle at all.
func fakeCgroup(t *testing.T, id, applied string) string {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("make the fake cgroup: %v", err)
	}

	for file, value := range map[string]string{"memory.max": applied, "memory.high": "max"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(value+"\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}

	return root
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func read(t *testing.T, root, id, file string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(root, id, file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}

	return strings.TrimSpace(string(raw))
}

// TestTheHostBoundsEndUpAboveTheGuestBound is the SHARD-97 fix itself: runsc leaves the cgroup at the
// bare bound, which charges the sentry against it, and create must move both knobs up.
func TestTheHostBoundsEndUpAboveTheGuestBound(t *testing.T) {
	const id = "amber-otter-1a2b"

	r := models.Resources{MemoryMiB: 128}
	root := fakeCgroup(t, id, "134217728")

	if err := gvisor.BoundMemory(root, models.SandboxSpec{ID: id, Resources: r}); err != nil {
		t.Fatalf("BoundMemory: %v", err)
	}

	if got, want := read(t, root, id, "memory.max"), gvisor.MemoryCeiling(r); got != itoa(want) {
		t.Errorf("memory.max = %s, want the ceiling %d", got, want)
	}
	if got, want := read(t, root, id, "memory.high"), gvisor.MemoryThrottle(r); got != itoa(want) {
		t.Errorf("memory.high = %s, want the throttle %d", got, want)
	}
}

// TestACgroupThatRunscDidNotBoundIsRefused covers the one that bites: runsc applies nothing at all
// when the directory is already there, and the sandbox would run with the whole host's memory.
func TestACgroupThatRunscDidNotBoundIsRefused(t *testing.T) {
	const id = "amber-otter-1a2b"

	root := fakeCgroup(t, id, "max")

	err := gvisor.BoundMemory(root, models.SandboxSpec{ID: id, Resources: models.Resources{MemoryMiB: 128}})
	if err == nil {
		t.Fatal("BoundMemory accepted a cgroup runsc left unbounded")
	}
	if !strings.Contains(err.Error(), "memory.max") {
		t.Errorf("the refusal is %q, which does not name the control file that holds the wrong number", err)
	}
}

func TestAnUnboundedSandboxTouchesNoCgroup(t *testing.T) {
	const id = "amber-otter-1a2b"

	root := fakeCgroup(t, id, "max")

	if err := gvisor.BoundMemory(root, models.SandboxSpec{ID: id}); err != nil {
		t.Fatalf("BoundMemory on a sandbox with no bound: %v", err)
	}
	if got := read(t, root, id, "memory.high"); got != "max" {
		t.Errorf("memory.high = %s, want the max an untouched cgroup holds", got)
	}
}

func TestABoundBelowTheSentryCostIsRefused(t *testing.T) {
	p := newProvider(t)

	spec := models.SandboxSpec{ID: "amber-otter-1a2b", Resources: models.Resources{MemoryMiB: gvisor.MinimumMemoryMiB - 1}}

	err := p.Create(t.Context(), spec)
	if err == nil {
		t.Fatal("Create accepted a bound the sentry cannot boot under")
	}
	if !strings.Contains(err.Error(), "at least") {
		t.Errorf("the refusal is %q, which does not name the smallest bound that works", err)
	}
}
