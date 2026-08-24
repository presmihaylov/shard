package bundle_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/models"
)

// TestTheShmTmpfsFitsInsideTheBound is the hole this closes: a tmpfs page is guest memory the host
// cgroup charges, so a mount larger than the bound lets a guest kill its own sandbox with dd.
func TestTheShmTmpfsFitsInsideTheBound(t *testing.T) {
	for _, boundMiB := range []int64{64, 128, 512, 4096} {
		_, spec := build(t, models.SandboxSpec{
			Entrypoint: []string{"/bin/sh"},
			Resources:  models.Resources{MemoryMiB: boundMiB},
		}, models.ImageConfig{})

		if got := tmpfsMiB(t, spec, "/dev/shm"); got > boundMiB {
			t.Errorf("a %d MiB sandbox may hold %d MiB of tmpfs, which is more than its own bound", boundMiB, got)
		}
	}
}

// TestAnUnboundedSandboxKeepsTheDockerShmSize pins the other side: nothing charges an unbounded
// sandbox, so shrinking its /dev/shm would cost a workload for no gain.
func TestAnUnboundedSandboxKeepsTheDockerShmSize(t *testing.T) {
	_, spec := build(t, models.SandboxSpec{Entrypoint: []string{"/bin/sh"}}, models.ImageConfig{})

	if got := tmpfsMiB(t, spec, "/dev/shm"); got != 64 {
		t.Errorf("/dev/shm is %d MiB in an unbounded sandbox, want 64", got)
	}
}

// TestDevIsReadOnly is the same hole on the mount gVisor will not size: it drops our size= on /dev
// and mounts its own devtmpfs, which reports half the host's memory to the guest.
func TestDevIsReadOnly(t *testing.T) {
	_, spec := build(t, models.SandboxSpec{
		Entrypoint: []string{"/bin/sh"},
		Resources:  models.Resources{MemoryMiB: 64},
	}, models.ImageConfig{})

	if !slices.Contains(mountAt(t, spec, "/dev").Options, "ro") {
		t.Error("/dev is writable, so a guest may fill it with the whole host and end its own sandbox")
	}
}

func tmpfsMiB(t *testing.T, spec specs.Spec, destination string) int64 {
	t.Helper()

	for _, option := range mountAt(t, spec, destination).Options {
		size, found := strings.CutPrefix(option, "size=")
		if !found {
			continue
		}

		mib, err := strconv.ParseInt(strings.TrimSuffix(size, "m"), 10, 64)
		if err != nil {
			t.Fatalf("the size of %s is %q, which is not a count of MiB: %v", destination, option, err)
		}

		return mib
	}

	t.Fatalf("the mount at %s carries no size, so the guest may fill it with the whole host", destination)

	return 0
}
