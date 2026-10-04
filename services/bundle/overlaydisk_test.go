package bundle_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/ext4"
	"github.com/presmihaylov/shard/services/bundle"
)

func TestWriteOverlayDiskIsAnEmptyExt4OfTheBound(t *testing.T) {
	dst := filepath.Join(t.TempDir(), bundle.OverlayDiskFile)

	if err := bundle.WriteOverlayDisk(dst, models.Resources{DiskMiB: 64}); err != nil {
		t.Fatalf("WriteOverlayDisk: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 64<<20 {
		t.Errorf("the disk is %d bytes, want the 64 MiB bound", info.Size())
	}
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	magic := make([]byte, 2)
	if _, err := f.ReadAt(magic, 1024+0x38); err != nil {
		t.Fatal(err)
	}
	if magic[0] != 0x53 || magic[1] != 0xef {
		t.Fatalf("magic %x", magic)
	}
	// Grow to the same size is a no-op that first checks the image is one the writer made.
	if err := ext4.Grow(dst, 64<<20); err != nil {
		t.Errorf("the disk is not an image ext4 wrote: %v", err)
	}
	if _, err := exec.LookPath("e2fsck"); err != nil {
		t.Logf("no e2fsck on PATH, the magic stands alone")

		return
	}
	if out, err := exec.Command("e2fsck", "-fn", dst).CombinedOutput(); err != nil {
		t.Fatalf("e2fsck: %v\n%s", err, out)
	}
}

func TestWriteOverlayDiskRefusesAnExistingTarget(t *testing.T) {
	dst := filepath.Join(t.TempDir(), bundle.OverlayDiskFile)
	if err := os.WriteFile(dst, []byte("taken"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := bundle.WriteOverlayDisk(dst, models.Resources{DiskMiB: 64}); err == nil {
		t.Fatal("WriteOverlayDisk replaced a disk that was there")
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "taken" {
		t.Errorf("the disk that was there reads %q, %v", got, err)
	}
}

// The providers refuse under this floor, so a bound they take never fails after the record.
func TestMinOverlayDiskMiBIsTheSmallestOverlay(t *testing.T) {
	dir := t.TempDir()

	if err := bundle.WriteOverlayDisk(filepath.Join(dir, "min.raw"), models.Resources{DiskMiB: bundle.MinOverlayDiskMiB}); err != nil {
		t.Fatalf("WriteOverlayDisk at the floor: %v", err)
	}
	if err := bundle.WriteOverlayDisk(filepath.Join(dir, "under.raw"), models.Resources{DiskMiB: bundle.MinOverlayDiskMiB - 1}); err == nil {
		t.Fatal("WriteOverlayDisk took a bound under the floor, so the floor is too high")
	}
}

func TestCheckGrowBoundAgreesWithTheGrow(t *testing.T) {
	for _, mib := range []int64{126, 127, 128, 129, 130, 131, 132, 254, 255, 256, 257, 258, 259, 260} {
		check := bundle.CheckGrowBound(mib)
		grow := bundle.WriteOverlayDisk(filepath.Join(t.TempDir(), bundle.OverlayDiskFile), models.Resources{DiskMiB: mib})
		if (check == nil) != (grow == nil) {
			t.Errorf("--disk %d: CheckGrowBound says %v, WriteOverlayDisk says %v", mib, check, grow)
		}
	}
}

func TestCheckGrowBoundNamesTheNearestBounds(t *testing.T) {
	for mib, want := range map[int64]string{129: "set resources.disk_mib to 128 MiB or 131 MiB", 130: "set resources.disk_mib to 128 MiB or 131 MiB", 258: "set resources.disk_mib to 256 MiB or 259 MiB"} {
		err := bundle.CheckGrowBound(mib)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckGrowBound(%d) = %v, want %q", mib, err, want)
		}
	}
}
