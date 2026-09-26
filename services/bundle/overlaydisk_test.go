package bundle_test

import (
	"os"
	"os/exec"
	"path/filepath"
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

func TestWriteOverlayDiskDefaultsTheBound(t *testing.T) {
	dst := filepath.Join(t.TempDir(), bundle.OverlayDiskFile)

	if err := bundle.WriteOverlayDisk(dst, models.Resources{}); err != nil {
		t.Fatalf("WriteOverlayDisk: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != bundle.DefaultDiskMiB<<20 {
		t.Errorf("the disk is %d bytes, want the default bound", info.Size())
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
