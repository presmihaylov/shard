package firecracker

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/services/bundle"
)

// vmmOverheadMiB is what firecracker holds on the host beside the guest's own memory: the vmm, its
// virtio queues, and the host page tables of the guest's mapping, which are ~2 MiB per GiB of guest.
const vmmOverheadMiB = 64

const bytesPerMiB = 1024 * 1024

// MemoryCeiling is memory.max on the vmm's host cgroup: the guest's whole memory and what firecracker
// holds beside it. The guest bounds itself 32 MiB under its own memory, so the guest's killer runs
// first on every workload; this one answers only a vmm that runs away, and it protects the host.
func MemoryCeiling(r models.Resources) int64 {
	return (r.MemoryMiB + vmmOverheadMiB) * bytesPerMiB
}

// bound makes the host cgroup a boot spawns this sandbox's vmm into. A provider without a cgroup
// root is a test one, on a host that has no cgroup v2 hierarchy: New always names the host's mount.
func (p *Provider) bound(id string, r models.Resources) (string, error) {
	if p.cgroupRoot == "" {
		return "", nil
	}

	return boundVMM(p.cgroupRoot, id, r)
}

// sweep drops the host cgroup of a sandbox that is gone, which nothing else on the host would empty.
// A killed vmm leaves its cgroup a moment after it stops answering, and the kernel refuses to remove
// one that still holds a task, so the sweep waits that moment out.
func (p *Provider) sweep(ctx context.Context, id string) error {
	if p.cgroupRoot == "" {
		return nil
	}

	dir := cgroupDir(p.cgroupRoot, id)
	deadline := time.Now().Add(killGrace)
	for {
		err := cgroup.Remove(dir)
		if err == nil || !time.Now().Before(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sweep the cgroup of sandbox %s: %w", id, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// boundVMM makes the host cgroup a sandbox's vmm is spawned into and returns its path.
func boundVMM(root, id string, r models.Resources) (string, error) {
	parent := filepath.Join(root, bundle.CgroupParent)
	if err := cgroup.Ensure(parent); err != nil {
		return "", err
	}
	// Nothing else makes this parent, so the controller it withholds is one no sandbox under it can bound.
	if err := cgroup.Delegate(parent, "memory"); err != nil {
		return "", fmt.Errorf("delegate the memory controller to the sandboxes: %w", err)
	}

	dir := cgroupDir(root, id)
	if err := cgroup.Ensure(dir); err != nil {
		return "", err
	}
	if err := cgroup.SetMemoryMax(dir, MemoryCeiling(r)); err != nil {
		return "", fmt.Errorf("bound the host memory of sandbox %s: %w", id, err)
	}
	// The guest's memory is one anonymous mapping, so a host that may swap it holds the whole VM on disk and the bound stops being one.
	if err := cgroup.SetMemorySwapMax(dir, 0); err != nil {
		return "", fmt.Errorf("pin the swap of sandbox %s to none: %w", id, err)
	}
	// Past the bound the whole sandbox dies, never a part of it, which is what the guest's own bound does too.
	if err := cgroup.SetOOMGroup(dir); err != nil {
		return "", fmt.Errorf("group the OOM kill of sandbox %s: %w", id, err)
	}

	return dir, nil
}

// cgroupDir is where a sandbox's vmm runs, under the one parent every shard sandbox sits in.
func cgroupDir(root, id string) string {
	return filepath.Join(root, bundle.CgroupsPath(id))
}
