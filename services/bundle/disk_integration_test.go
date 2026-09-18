//go:build integration

package bundle_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/presmihaylov/shard/pkg/mountinfo"
)

const boundMiB = 64

// TestAWritePastTheDiskBoundFailsInTheGuest is the SHARD-173 acceptance criterion, on /tmp and on the root alike.
func TestAWritePastTheDiskBoundFailsInTheGuest(t *testing.T) {
	requireRunsc(t)

	// Each dd asks for three times the bound, and a full disk takes no status file, so the counts wait in the shell until the fill is gone.
	script := `t=$(dd if=/dev/zero of=/tmp/fill bs=1M count=192 2>&1 | grep -c "No space left on device"); rm -f /tmp/fill; ` +
		`r=$(dd if=/dev/zero of=/fill bs=1M count=192 2>&1 | grep -c "No space left on device"); rm -f /fill; ` +
		`echo "tmp=$t root=$r" > /root/enospc`
	b, lower := buildBoundedBundle(t, t.TempDir(), boundMiB, []string{"/bin/sh", "-c", script})
	if err := b.Mount(lower); err != nil {
		t.Fatalf("mount the overlay: %v", err)
	}
	t.Cleanup(func() { b.Unmount() })

	runSandbox(t, b, "shard-173-fill")

	if got := strings.TrimSpace(readFile(t, filepath.Join(b.Upper, "root/enospc"))); got != "tmp=1 root=1" {
		t.Errorf("the guest saw %q, want ENOSPC once on /tmp and once on the root", got)
	}

	info, err := os.Stat(b.Image)
	if err != nil {
		t.Fatalf("stat the disk image: %v", err)
	}
	if info.Size() != boundMiB<<20 {
		t.Errorf("the disk image is %d bytes, want the bound %d", info.Size(), boundMiB<<20)
	}
	// The image is sparse, so what the host holds is what the guest wrote, and the bound is its ceiling.
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat of %s carries no Stat_t", b.Image)
	}
	if held := stat.Blocks * 512; held > info.Size() {
		t.Errorf("the host holds %d bytes of the disk image, want at most the bound %d", held, info.Size())
	}
}

// TestUnmountDetachesTheDisk pins what a stop leaves: no overlay and no loop device, so the directory can go.
func TestUnmountDetachesTheDisk(t *testing.T) {
	requireRunsc(t)

	b, lower := buildBoundedBundle(t, t.TempDir(), boundMiB, []string{"/bin/true"})
	if err := b.Mount(lower); err != nil {
		t.Fatalf("mount the overlay: %v", err)
	}
	if _, found, err := mountinfo.At(b.Disk); err != nil || !found {
		t.Fatalf("the disk is not mounted at %s after Mount (found %v, err %v)", b.Disk, found, err)
	}

	if err := b.Unmount(); err != nil {
		t.Fatalf("unmount: %v", err)
	}
	for _, point := range []string{b.RootFS, b.Disk} {
		if _, found, err := mountinfo.At(point); err != nil || found {
			t.Errorf("%s is still mounted after the unmount (found %v, err %v)", point, found, err)
		}
	}
}
