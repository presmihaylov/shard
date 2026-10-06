package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

// A holder notes who it is over what an earlier, longer note left, on the file the lock is held on.
func TestNoteReplacesTheLockFileContentInPlace(t *testing.T) {
	path := lockPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("/var/lib/an-earlier-and-longer-root\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	l, err := TryAcquire(path, 0o600)
	if err != nil || l == nil {
		t.Fatalf("TryAcquire: %v, %v", l, err)
	}
	t.Cleanup(func() {
		if err := l.Release(); err != nil {
			t.Errorf("Release: %v", err)
		}
	})
	if err := l.Note([]byte("/srv/b\n")); err != nil {
		t.Fatalf("Note: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "/srv/b\n" {
		t.Errorf("the lock file holds %q, want /srv/b alone", got)
	}
	// Still held: a note that renamed the file would free the path for anyone.
	if again, err := TryAcquire(path, 0o600); err != nil || again != nil {
		t.Errorf("TryAcquire after the note got %v, %v, want the lock still held", again, err)
	}
}

func TestAcquireWaitsForARelease(t *testing.T) {
	path := lockPath(t)

	first, err := TryAcquire(path, 0o600)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}

	won := make(chan *Lock, 1)
	go func() {
		l, err := Acquire(path, 0o600, 2*time.Second)
		if err != nil {
			t.Errorf("Acquire: %v", err)
		}
		won <- l
	}()

	// The waiter must not win while the first holder still holds the lock.
	select {
	case <-won:
		t.Fatal("Acquire won a lock another holder still held")
	case <-time.After(50 * time.Millisecond):
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	second := <-won
	if second == nil {
		t.Fatal("Acquire never won the lock after the release")
	}
	if err := second.Release(); err != nil {
		t.Fatalf("Release the second lock: %v", err)
	}
}

func TestAcquireTimesOutOnAHeldLock(t *testing.T) {
	path := lockPath(t)

	first, err := TryAcquire(path, 0o600)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}

	if _, err := Acquire(path, 0o600, 20*time.Millisecond); err == nil {
		t.Fatal("Acquire won a lock that was held for its whole timeout")
	}

	if err := first.Release(); err != nil {
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

func TestAcquireDirWaitsForTheHolderAndAddsNoFile(t *testing.T) {
	dir := t.TempDir()

	first, err := AcquireDir(dir, time.Second)
	if err != nil {
		t.Fatalf("AcquireDir: %v", err)
	}
	if _, err := AcquireDir(dir, 20*time.Millisecond); err == nil {
		t.Fatal("AcquireDir won a directory that was held for its whole timeout")
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	second, err := AcquireDir(dir, time.Second)
	if err != nil {
		t.Fatalf("AcquireDir after the release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("Release the second lock: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("AcquireDir left %d entries in the directory, want none", len(entries))
	}
}
