package store

import (
	"os"
	"path/filepath"
	"testing"
)

func lockPath(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "sub", ".lock")
}

func TestTryAcquireCreatesTheFileAndItsDirectory(t *testing.T) {
	path := lockPath(t)

	l, err := TryAcquire(path, 0o600)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if l == nil {
		t.Fatal("a free lock was reported as held")
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat the lock file: %v", err)
	}

	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestTryAcquireReportsAHeldLock(t *testing.T) {
	path := lockPath(t)

	first, err := TryAcquire(path, 0o600)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}

	second, err := TryAcquire(path, 0o600)
	if err != nil {
		t.Fatalf("the second TryAcquire: %v", err)
	}
	if second != nil {
		t.Fatal("two holders took the same lock")
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	third, err := TryAcquire(path, 0o600)
	if err != nil {
		t.Fatalf("TryAcquire after the release: %v", err)
	}
	if third == nil {
		t.Fatal("a released lock still reads as held")
	}

	if err := third.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestTwoPathsDoNotBlockEachOther(t *testing.T) {
	dir := t.TempDir()

	first, err := TryAcquire(filepath.Join(dir, "one.lock"), 0o600)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}

	second, err := TryAcquire(filepath.Join(dir, "two.lock"), 0o600)
	if err != nil {
		t.Fatalf("the second TryAcquire: %v", err)
	}
	if second == nil {
		t.Fatal("a second path read as held")
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}
