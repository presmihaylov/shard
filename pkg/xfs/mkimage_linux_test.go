package xfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// A mkfs.xfs stand-in: it fails while FAKE_MKFS_FAIL is set, and writes the superblock magic otherwise.
const fakeMkfs = `#!/bin/sh
if [ -n "$FAKE_MKFS_FAIL" ]; then exit 1; fi
printf XFSB > "$4"
`

func TestMakeImageLeavesNothingAtThePathUntilTheFormatSucceeds(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, Mkfs), []byte(fakeMkfs), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("FAKE_MKFS_FAIL", "1")
	image := filepath.Join(t.TempDir(), "shard.xfs")

	if err := MakeImage(t.Context(), image, 1<<20); err == nil {
		t.Fatal("a failed format returned nil")
	}
	for _, p := range []string{image, image + ".part"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is left after a failed format: %v", p, err)
		}
	}

	t.Setenv("FAKE_MKFS_FAIL", "")
	if err := MakeImage(t.Context(), image, 1<<20); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if ok, err := IsImage(image); err != nil || !ok {
		t.Errorf("after the retry %s is %v, %v", image, ok, err)
	}
	if _, err := os.Stat(image + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging file outlived the rename: %v", err)
	}
}

// A crashed format leaves its staging file behind, and MakeImage removes it first, so its blocks count as room.
func TestRoomCountsTheStagingFileOfACrashedFormat(t *testing.T) {
	// Other writers on the disk move its free count between two reads, so the test pins it.
	const free = 1 << 30
	statfs = func(dir string, fs *unix.Statfs_t) error {
		if err := unix.Statfs(dir, fs); err != nil {
			return err
		}
		fs.Bavail, fs.Bsize = free/4096, 4096

		return nil
	}
	t.Cleanup(func() { statfs = unix.Statfs })

	image := filepath.Join(t.TempDir(), "shard.xfs")
	before, err := Room(image)
	if err != nil || before != free {
		t.Fatalf("room beside a fresh image: %d, %v; want %d", before, err, free)
	}
	if err := reserve(image+stagingSuffix, 64<<20); err != nil {
		t.Fatal(err)
	}

	after, err := Room(image)
	if err != nil {
		t.Fatal(err)
	}
	if staged := after - before; staged < 64<<20 {
		t.Errorf("room grew by %d bytes across a 64 MiB staging file, want at least %d", staged, 64<<20)
	}

	if _, err := Room(filepath.Join(t.TempDir(), "missing", "shard.xfs")); err == nil {
		t.Error("room under a missing directory returned nil")
	}
}
