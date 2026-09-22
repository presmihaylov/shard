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
