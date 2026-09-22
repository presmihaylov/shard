//go:build integration

package datadir

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/reflink"
	"github.com/presmihaylov/shard/pkg/xfs"
)

// A live bootstrap: an ext4 or tmpfs root gets a loopback xfs image beside it, and a clone under it shares blocks.
func TestEnsureProvisionsALoopbackXFSImage(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for mkfs.xfs and mount")
	}
	if err := xfs.Have(); err != nil {
		t.Skip(err)
	}

	base := t.TempDir()
	if fs, err := reflink.Probe(base); err != nil || fs.Reflink {
		t.Skipf("%s already clones (%+v, %v); the bootstrap has nothing to prove here", base, fs, err)
	}
	dir := filepath.Join(base, "shard")
	xfs.FstabPath = filepath.Join(base, "fstab")
	if err := os.WriteFile(xfs.FstabPath, []byte("# static\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("umount", dir).CombinedOutput(); err != nil {
			t.Errorf("umount %s: %v: %s", dir, err, bytes.TrimSpace(out))
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	// The smallest image mkfs.xfs accepts is 300 MiB.
	cfg := Config{Dir: dir, Provider: Firecracker, ImageMiB: 320}
	if err := Ensure(ctx, cfg); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if err := Ensure(ctx, cfg); err != nil {
		t.Fatalf("second ensure is not idempotent: %v", err)
	}

	fs, err := reflink.Probe(dir)
	if err != nil || fs.Type != "xfs" || !fs.Reflink {
		t.Fatalf("after the bootstrap %s is %+v, %v", dir, fs, err)
	}
	fstab, err := os.ReadFile(xfs.FstabPath)
	if err != nil || bytes.Count(fstab, []byte(dir+" xfs loop")) != 1 {
		t.Errorf("fstab holds %q, %v", fstab, err)
	}

	src := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(src, bytes.Repeat([]byte("shard"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reflink.Clone(src, filepath.Join(dir, "clone.img")); err != nil {
		t.Fatalf("a clone on the new root: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "clone.img"))
	if err != nil || !bytes.Equal(got, bytes.Repeat([]byte("shard"), 1<<20)) {
		t.Errorf("the clone reads back wrong: %d bytes, %v", len(got), err)
	}
}
