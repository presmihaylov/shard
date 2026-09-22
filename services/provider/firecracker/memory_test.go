package firecracker_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/provider/firecracker"
)

const mib = 1024 * 1024

// The host bound sits above the guest's whole memory, so the guest's own killer runs first on every
// workload and the host's answers only a vmm that runs away. A ceiling at or under it would end the
// VM before the guest could report the kill, and the daemon would read a death, not an OOM.
func TestTheHostBoundSitsAboveTheGuestsWholeMemory(t *testing.T) {
	for _, memoryMiB := range []int64{128, 512, 4096} {
		got := firecracker.MemoryCeiling(models.Resources{MemoryMiB: memoryMiB})
		if got <= memoryMiB*mib {
			t.Errorf("a %d MiB guest is bounded at %d bytes on the host, which is not above its own memory", memoryMiB, got)
		}
	}
}

func TestTheVMMIsBoundedOnACgroupOfItsOwn(t *testing.T) {
	root := t.TempDir()

	dir, err := firecracker.BoundVMM(root, "abcdef012345", models.Resources{MemoryMiB: 512})
	if err != nil {
		t.Fatalf("BoundVMM: %v", err)
	}

	if want := filepath.Join(root, "shard", "abcdef012345"); dir != want {
		t.Errorf("the vmm is bounded on %s, want %s", dir, want)
	}
	// 512 MiB of guest and 64 MiB of vmm overhead, in bytes.
	for file, want := range map[string]string{
		"memory.max":       "603979776",
		"memory.swap.max":  "0",
		"memory.oom.group": "1",
	} {
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if string(raw) != want {
			t.Errorf("%s = %q, want %q", file, raw, want)
		}
	}

	// Nothing else makes the parent, so a sandbox under it holds no memory bound until this write lands.
	raw, err := os.ReadFile(filepath.Join(root, "shard", "cgroup.subtree_control"))
	if err != nil {
		t.Fatalf("read the parent's subtree_control: %v", err)
	}
	if !strings.Contains(string(raw), "memory") {
		t.Errorf("the parent delegates %q, want the memory controller", raw)
	}
}
