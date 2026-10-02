package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const gib = 1024 * bytesPerMiB

func TestFitsCountsEveryHeldDiskAtItsBound(t *testing.T) {
	cases := map[string]struct {
		bound, held, free int64
		fits              bool
	}{
		"a disk over the free space":         {bound: 4 * gib, free: 2 * gib},
		"a disk under the free space":        {bound: gib, free: 2 * gib, fits: true},
		"a disk that leaves the headroom":    {bound: 2*gib - diskHeadroom, free: 2 * gib, fits: true},
		"a disk that eats into the headroom": {bound: 2*gib - diskHeadroom + 1, free: 2 * gib},
		"a disk the others already hold":     {bound: gib, held: gib, free: 2 * gib},
		"a root under the headroom":          {bound: 1, free: diskHeadroom - 1},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := fits(c.bound, c.held, c.free)
			if c.fits && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !c.fits && err == nil {
				t.Fatal("admitted")
			}
		})
	}
}

func TestFitsNamesTheSizes(t *testing.T) {
	err := fits(4*gib, gib, 2*gib)
	if err == nil {
		t.Fatal("admitted")
	}
	for _, want := range []string{"4096 MiB disk", "2048 MiB free", "1024 MiB", "64 MiB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not name %q", err, want)
		}
	}
}

func sparse(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// A stopped sandbox is counted like a running one, and a directory with no disk yet is not.
func TestHeldDisksSumsTheOtherSandboxes(t *testing.T) {
	sandboxes := t.TempDir()
	sparse(t, filepath.Join(sandboxes, "running", "disk.img"), gib)
	sparse(t, filepath.Join(sandboxes, "stopped", "disk.img"), 2*gib)
	sparse(t, filepath.Join(sandboxes, "self", "disk.img"), 8*gib)
	sparse(t, filepath.Join(sandboxes, "loose.img"), 8*gib)
	if err := os.Mkdir(filepath.Join(sandboxes, "claimed"), 0o750); err != nil {
		t.Fatal(err)
	}

	held, err := heldDisks(sandboxes, "disk.img", filepath.Join(sandboxes, "self"))
	if err != nil {
		t.Fatal(err)
	}
	if held != 3*gib {
		t.Errorf("held %d MiB, want 3072", held/bytesPerMiB)
	}
}

func TestAdmitDiskRefusesWithoutWriting(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "sandboxes", "new", "disk.img")
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		t.Fatal(err)
	}

	wrote := false
	err := admitDisk(dst, 1<<50, func() error {
		wrote = true

		return nil
	})
	if err == nil {
		t.Fatal("admitted a 1 PiB disk")
	}
	if wrote {
		t.Error("wrote the disk it refused")
	}
}

func TestAdmitDiskWritesWhatFits(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "sandboxes", "new", "disk.img")
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		t.Fatal(err)
	}

	wrote := false
	if err := admitDisk(dst, bytesPerMiB, func() error {
		wrote = true

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Error("admitted the disk but did not write it")
	}
}

// A clone keeps the size of what it copies, so that size is the bound admitted.
func TestAdmitCopyTakesTheBoundFromTheSource(t *testing.T) {
	sandboxes := filepath.Join(t.TempDir(), "sandboxes")
	src := filepath.Join(sandboxes, "source", "disk.img")
	sparse(t, src, 8<<40)
	dst := filepath.Join(sandboxes, "copy", "disk.img")
	if err := os.Mkdir(filepath.Dir(dst), 0o750); err != nil {
		t.Fatal(err)
	}

	err := AdmitCopy(src, dst, func() error {
		t.Error("wrote the copy it refused")

		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "8388608 MiB disk") {
		t.Fatalf("got %v, want a refusal of the 8 TiB copy", err)
	}
}
