package bundle

import (
	"os"
	"path/filepath"
	"testing"
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
