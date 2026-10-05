package bundle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/pkg/ext4"
)

// A copy that fails midway leaves no half-written target, so a retry can take the name.
func TestCopyFileRemovesAFailedCopy(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "rootfs.ext4")

	if err := copyFile(dir, dst); err == nil {
		t.Fatal("copied a directory")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("the failed copy left %s: %v", dst, err)
	}

	src := filepath.Join(dir, "base.ext4")
	if err := os.WriteFile(src, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("the retry: %v", err)
	}
}

// A guest mount can take the descriptor block a larger disk needs, so the refusal names the largest --disk that still grows.
func TestSeedRefusalNamesTheLargestDiskThatGrows(t *testing.T) {
	taken := &ext4.DescriptorTakenError{Block: 2, Max: 16 << 30}

	err := seedRefusal(fmt.Errorf("ext4: grow overlay.raw: %w", taken), 20480)
	var got *ext4.DescriptorTakenError
	if !errors.As(err, &got) {
		t.Fatalf("seedRefusal = %v, want the taken descriptor block under it", err)
	}
	if !strings.Contains(err.Error(), "at most 16384 MiB") || !strings.Contains(err.Error(), "set resources.disk_mib to 16384 MiB or less") {
		t.Errorf("seedRefusal = %v, want the 16384 MiB that still grows", err)
	}
}
