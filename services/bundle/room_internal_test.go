package bundle

import (
	"errors"
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
	if err := writeSparse(path, size); err != nil {
		t.Fatal(err)
	}
}

func writeSparse(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}

	return errors.Join(f.Truncate(size), f.Close())
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

// A copy keeps the size of its source, so that size is the bound admitted.
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

// A resume drops its disk before it clones the save back, and an admission in that gap would miss the disk's bound (SHARD-393).
func TestAnAdmissionWaitsOutAReplace(t *testing.T) {
	sandboxes := filepath.Join(t.TempDir(), "sandboxes")
	paused := filepath.Join(sandboxes, "paused", "disk.img")
	sparse(t, paused, 8<<40)
	dst := filepath.Join(sandboxes, "new", "disk.img")
	if err := os.Mkdir(filepath.Dir(dst), 0o750); err != nil {
		t.Fatal(err)
	}

	dropped, release := make(chan struct{}), make(chan struct{})
	replaced := make(chan error, 1)
	go func() {
		replaced <- ReplaceDisk(func() error {
			if err := os.Remove(paused); err != nil {
				return err
			}
			close(dropped)
			<-release

			return writeSparse(paused, 8<<40)
		})
	}()
	<-dropped
	if admitting.TryLock() {
		admitting.Unlock()
		t.Fatal("the admission lock is free while the paused disk is gone")
	}

	admitted := make(chan error, 1)
	go func() {
		admitted <- admitDisk(dst, bytesPerMiB, func() error { return nil })
	}()
	close(release)
	if err := <-replaced; err != nil {
		t.Fatal(err)
	}
	if err := <-admitted; err == nil {
		t.Fatal("admitted a disk beside the 8 TiB one the replace put back")
	}
}

// reserve stands in for a Reserve whose bound the host could never hold, so the test owns every number.
func reserve(t *testing.T, dir string, bound int64) {
	t.Helper()
	admitting.Lock()
	reserved[dir] = bound
	admitting.Unlock()
	t.Cleanup(func() { Release(dir) })
}

func reservation(t *testing.T, dir string) (int64, bool) {
	t.Helper()
	admitting.Lock()
	defer admitting.Unlock()
	bound, ok := reserved[dir]

	return bound, ok
}

// A create reserves its disk before the record, so an admission while that disk is not yet written still counts it (SHARD-393).
func TestAReservationCountsUntilItIsReleased(t *testing.T) {
	sandboxes := filepath.Join(t.TempDir(), "sandboxes")
	first := filepath.Join(sandboxes, "first")
	dst := filepath.Join(sandboxes, "second", "disk.img")
	for _, dir := range []string{first, filepath.Dir(dst)} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	reserve(t, first, 8<<40)

	if err := Reserve(dst, bytesPerMiB); err == nil {
		t.Fatal("reserved a disk beside the 8 TiB one the first create reserved")
	}
	if err := admitDisk(dst, bytesPerMiB, func() error { return nil }); err == nil {
		t.Fatal("admitted a disk beside the 8 TiB one the first create reserved")
	}

	Release(first)
	if err := Reserve(dst, bytesPerMiB); err != nil {
		t.Fatalf("a release did not give the reservation back: %v", err)
	}
	t.Cleanup(func() { Release(filepath.Dir(dst)) })
}

// The disk write takes the reservation without a second check, which would count the guest's own writes against it.
func TestAWriteTakesItsReservation(t *testing.T) {
	sandboxes := filepath.Join(t.TempDir(), "sandboxes")
	dst := filepath.Join(sandboxes, "new", "disk.img")
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		t.Fatal(err)
	}
	// Only the reservation lets this past the check: no host holds 8 TiB free.
	reserve(t, filepath.Dir(dst), 8<<40)

	wrote := false
	if err := admitDisk(dst, 8<<40, func() error {
		wrote = true

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Error("the write did not run")
	}
	if _, ok := reservation(t, filepath.Dir(dst)); ok {
		t.Error("the reservation outlived the disk it was for")
	}
}

// A write past what was reserved is checked again, and the reservation goes either way.
func TestAWritePastItsReservationIsChecked(t *testing.T) {
	sandboxes := filepath.Join(t.TempDir(), "sandboxes")
	dst := filepath.Join(sandboxes, "new", "disk.img")
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		t.Fatal(err)
	}
	reserve(t, filepath.Dir(dst), bytesPerMiB)

	if err := admitDisk(dst, 8<<40, func() error { return nil }); err == nil {
		t.Fatal("admitted an 8 TiB disk on a 1 MiB reservation")
	}
	if _, ok := reservation(t, filepath.Dir(dst)); ok {
		t.Error("the reservation outlived the refused write")
	}
}

// A checkpoint's memory is refused by name when it does not fit, and the write never runs.
func TestAdmitMemoryRefusesWithoutWriting(t *testing.T) {
	disk := filepath.Join(t.TempDir(), "sandboxes", "source", "disk.img")
	sparse(t, disk, bytesPerMiB)

	err := AdmitMemory(disk, 1<<50, func() error {
		t.Error("wrote the memory it refused")

		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "memory of the checkpoint does not fit") {
		t.Fatalf("got %v, want the 1 PiB memory refused by name", err)
	}
}

// A memory write in flight is not in the free space yet, so a disk admitted meanwhile counts it (SHARD-562).
func TestAMemoryWriteCountsUntilItEnds(t *testing.T) {
	sandboxes := filepath.Join(t.TempDir(), "sandboxes")
	source := filepath.Join(sandboxes, "source", "disk.img")
	sparse(t, source, bytesPerMiB)
	dst := filepath.Join(sandboxes, "new", "disk.img")
	if err := os.Mkdir(filepath.Dir(dst), 0o750); err != nil {
		t.Fatal(err)
	}
	free, err := freeBytes(sandboxes)
	if err != nil {
		t.Fatal(err)
	}
	room := free - diskHeadroom - bytesPerMiB
	if room < gib {
		t.Skipf("the root has %d MiB free, too little to split", free/bytesPerMiB)
	}
	// Half the room for the memory and three quarters for the disk fit one at a time, never both, and leave a margin for the host's own writes.
	memory, bound := room/2, room/4*3

	writing, release := make(chan struct{}), make(chan struct{})
	written := make(chan error, 1)
	go func() {
		written <- AdmitMemory(source, memory, func() error {
			close(writing)
			<-release

			return nil
		})
	}()
	<-writing
	if err := admitDisk(dst, bound, func() error { return nil }); err == nil {
		t.Fatal("admitted a disk into the room the memory write is filling")
	}

	close(release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := admitDisk(dst, bound, func() error { return nil }); err != nil {
		t.Fatalf("the ended write still holds its room: %v", err)
	}
}
