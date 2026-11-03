//go:build integration

package bundle_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/presmihaylov/shard/models"
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
	b, lower := buildBoundedBundle(t, t.TempDir(), boundMiB)
	if err := b.Mount(lower); err != nil {
		t.Fatalf("mount the overlay: %v", err)
	}
	t.Cleanup(func() { b.Unmount() })

	runSandbox(t, b, "shard-173-fill", "/bin/sh", "-c", script)

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

	b, lower := buildBoundedBundle(t, t.TempDir(), boundMiB)
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

// TestUnmountOverlayKeepsTheDisk pins what a Sysbox stop leaves: no overlay, and the disk up for sysbox-mgr to chown the upper layer back at delete.
func TestUnmountOverlayKeepsTheDisk(t *testing.T) {
	requireRunsc(t)

	b, lower := buildBoundedBundle(t, t.TempDir(), boundMiB)
	if err := b.Mount(lower); err != nil {
		t.Fatalf("mount the overlay: %v", err)
	}
	t.Cleanup(func() { b.Unmount() })

	if err := b.UnmountOverlay(); err != nil {
		t.Fatalf("unmount the overlay: %v", err)
	}
	if _, found, err := mountinfo.At(b.RootFS); err != nil || found {
		t.Errorf("%s is still mounted after the overlay unmount (found %v, err %v)", b.RootFS, found, err)
	}
	if _, found, err := mountinfo.At(b.Disk); err != nil || !found {
		t.Errorf("the disk is not mounted at %s after the overlay unmount (found %v, err %v)", b.Disk, found, err)
	}
	if _, err := os.Stat(b.Upper); err != nil {
		t.Errorf("the upper layer is out of reach after the overlay unmount: %v", err)
	}
}

// TestResizeGrowsTheDiskOnline is the OCI half of SHARD-813: the image, the ext4 on it and the bound grow, mounted, and the files stay.
func TestResizeGrowsTheDiskOnline(t *testing.T) {
	requireRunsc(t)

	b, _ := buildBoundedBundle(t, t.TempDir(), boundMiB)
	marker := filepath.Join(b.Disk, "marker")
	write(t, marker, "kept")

	if err := b.Resize(models.Resources{DiskMiB: 2 * boundMiB}); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	if got := sizeOf(t, b.Image); got != 2*boundMiB<<20 {
		t.Errorf("the disk image is %d bytes, want %d", got, 2*boundMiB<<20)
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(b.Disk, &fs); err != nil {
		t.Fatalf("statfs %s: %v", b.Disk, err)
	}
	// The ext4 keeps its own metadata, so it reads short of the image, but well past the old bound.
	if got := fs.Blocks * uint64(fs.Bsize); got <= boundMiB<<20 {
		t.Errorf("the mounted ext4 holds %d bytes after the grow, want more than the old %d", got, boundMiB<<20)
	}
	if got := readFile(t, marker); got != "kept" {
		t.Errorf("the marker reads %q after the grow, want kept", got)
	}
	if got := diskBoundOf(t, b); got != 2*boundMiB {
		t.Errorf("the next start reads a %d MiB disk bound, want %d", got, 2*boundMiB)
	}
}
