package xfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
	image := filepath.Join(t.TempDir(), "shard.xfs")
	before, err := Room(image)
	if err != nil || before <= 0 {
		t.Fatalf("room beside a fresh image: %d, %v", before, err)
	}
	if err := reserve(image+stagingSuffix, 64<<20); err != nil {
		t.Fatal(err)
	}

	after, err := Room(image)
	if err != nil {
		t.Fatal(err)
	}
	// Other writers on the host move the free space a little between the two reads.
	if diff := after - before; diff < -(4<<20) || diff > 4<<20 {
		t.Errorf("room moved by %d bytes across a 64 MiB staging file, want about zero", diff)
	}

	if _, err := Room(filepath.Join(t.TempDir(), "missing", "shard.xfs")); err == nil {
		t.Error("room under a missing directory returned nil")
	}
}
