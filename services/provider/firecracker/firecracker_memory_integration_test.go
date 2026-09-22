//go:build integration && linux

package firecracker_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/firecracker"
)

// Guest memory is a hard cap the host never sees move: firecracker holds the whole mapping from the
// boot, so a workload that exhausts it is killed by the guest kernel and the host footprint stays
// flat. The kill therefore reaches the daemon only over vsock, and this is what restart_on_oom reads.
func TestAGuestThatOutgrowsItsBoundIsOOMKilled(t *testing.T) {
	h := newVMHarness(t)

	// The tmpfs is charged to the writer, and its default size sits under the bound, so the remount lifts it first.
	script := "mount -o remount,size=1G /dev/shm && dd if=/dev/zero of=/dev/shm/fill bs=1M; while true; do sleep 1; done"
	spec := h.newSpec(t, "/bin/sh", "-c", script)
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Minute)
	for {
		status, err := h.provider.Status(t.Context(), spec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == models.StateStopped {
			if !status.OOMKilled {
				log, _ := os.ReadFile(filepath.Join(spec.StateDir, "output.log"))
				t.Fatalf("the guest stopped without the bound blamed\nsandbox log:\n%s", log)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the guest is still %s two minutes into the fill", status.State)
		}
		time.Sleep(time.Second)
	}
}

// The host bound answers a vmm that runs away, never the workload: it sits above the guest's whole
// memory, so the guest's own killer always fires first and the daemon hears an OOM, not a death.
func TestTheVMMRunsUnderAHostBound(t *testing.T) {
	h := newVMHarness(t)

	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(cgroup.Root, bundle.CgroupsPath(spec.ID))
	max, err := cgroup.MemoryMax(dir)
	if err != nil {
		t.Fatalf("read the ceiling of the vmm: %v", err)
	}
	if want := firecracker.MemoryCeiling(spec.Resources); max != want {
		t.Errorf("the vmm is bounded at %d bytes, want %d", max, want)
	}
	if max <= spec.Resources.MemoryMiB<<20 {
		t.Errorf("the vmm is bounded at %d bytes, which is not above the %d MiB the guest holds", max, spec.Resources.MemoryMiB)
	}
	for file, want := range map[string]string{"memory.swap.max": "0", "memory.oom.group": "1"} {
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if got := strings.TrimSpace(string(raw)); got != want {
			t.Errorf("%s = %s, want %s", file, got, want)
		}
	}

	pids, err := cgroup.Procs(dir)
	if err != nil {
		t.Fatalf("list the processes of the vmm cgroup: %v", err)
	}
	if len(pids) == 0 {
		t.Error("the cgroup holds no process, so the vmm is charged somewhere else")
	}

	if err := h.provider.Remove(t.Context(), spec.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the cgroup outlived the sandbox: %v", err)
	}
}
