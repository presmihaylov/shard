package cgroup_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/pkg/cgroup"
)

// fake stands in for a cgroup directory: the control files are ordinary files, and the driver only
// ever writes to one that already exists.
func fake(t *testing.T, contents string) string {
	t.Helper()

	dir := t.TempDir()
	for _, file := range []string{"memory.high", "memory.max"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(contents), 0o600); err != nil {
			t.Fatalf("write the control file: %v", err)
		}
	}

	return dir
}

func TestSetAndReadTheThrottle(t *testing.T) {
	dir := fake(t, "max\n")

	if err := cgroup.SetMemoryHigh(dir, 134217728); err != nil {
		t.Fatalf("SetMemoryHigh: %v", err)
	}

	got, err := cgroup.MemoryHigh(dir)
	if err != nil {
		t.Fatalf("MemoryHigh: %v", err)
	}
	if got != 134217728 {
		t.Fatalf("MemoryHigh = %d, want 134217728", got)
	}
}

func TestMaxReadsAsNoThrottle(t *testing.T) {
	got, err := cgroup.MemoryHigh(fake(t, "max\n"))
	if err != nil {
		t.Fatalf("MemoryHigh: %v", err)
	}
	if got != -1 {
		t.Fatalf("MemoryHigh = %d, want -1 for max", got)
	}
}

func TestACgroupThatIsGoneIsNamedAsSuch(t *testing.T) {
	err := cgroup.SetMemoryHigh(filepath.Join(t.TempDir(), "nothing"), 1)
	if !errors.Is(err, cgroup.ErrNotFound) {
		t.Fatalf("SetMemoryHigh on a missing cgroup = %v, want ErrNotFound", err)
	}
}

func TestReadTheCeiling(t *testing.T) {
	got, err := cgroup.MemoryMax(fake(t, "201326592\n"))
	if err != nil {
		t.Fatalf("MemoryMax: %v", err)
	}
	if got != 201326592 {
		t.Fatalf("MemoryMax = %d, want 201326592", got)
	}
}

func TestMaxReadsAsNoCeiling(t *testing.T) {
	got, err := cgroup.MemoryMax(fake(t, "max\n"))
	if err != nil {
		t.Fatalf("MemoryMax: %v", err)
	}
	if got != -1 {
		t.Fatalf("MemoryMax = %d, want -1 for max", got)
	}
}

func TestACeilingThatIsNotANumberIsAFailure(t *testing.T) {
	if _, err := cgroup.MemoryMax(fake(t, "not a number\n")); err == nil {
		t.Fatal("MemoryMax on a control file that holds no number = nil, want an error")
	}
}
