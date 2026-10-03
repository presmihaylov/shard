package bundle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/models"
)

// The writer goes past admission, which refuses the 10 GiB default on a host with less free.
func TestWriteOverlayDiskDefaultsTheBound(t *testing.T) {
	dst := filepath.Join(t.TempDir(), OverlayDiskFile)

	if err := writeOverlayDisk(dst, models.Resources{}); err != nil {
		t.Fatalf("writeOverlayDisk: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != DefaultDiskMiB<<20 {
		t.Errorf("the disk is %d bytes, want the default bound", info.Size())
	}
}
