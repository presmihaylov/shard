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

// inBackground reports when the background holder takes the lock, and joins before the test ends,
// so a failure it reports never lands after the test has finished.
func inBackground(t *testing.T, path string) <-chan struct{} {
	t.Helper()

	held, done := make(chan struct{}), make(chan struct{})

	go func() {
		defer close(done)

		l, err := Acquire(path, 0o600)
		if err != nil {
			t.Errorf("the background lock: %v", err)

			return
		}

		close(held)

		if err := l.Release(); err != nil {
			t.Errorf("Release: %v", err)
		}
	}()

	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the background holder never finished")
		}
	})

	return held
}

func TestAcquireCreatesTheFileAndItsDirectory(t *testing.T) {
	l, err := Acquire(lockPath(t), 0o600)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestTheExclusiveLockBlocksASecondHolder(t *testing.T) {
	path := lockPath(t)

	first, err := Acquire(path, 0o600)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	held := inBackground(t, path)

	select {
	case <-held:
		t.Fatal("the second Acquire went through while the first held the lock")
	case <-time.After(50 * time.Millisecond):
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the second Acquire never went through after the release")
	}
}

// A delete unlinks the lock file while it holds it, so the waiter wakes on an inode with no name.
// It must move onto the file that has the name, or it excludes nobody.
func TestALockFileThatWasUnlinkedStillExcludes(t *testing.T) {
	path := lockPath(t)

	first, err := Acquire(path, 0o600)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	errs := make(chan error, 2)
	held := make(chan *Lock, 1)

	go func() {
		l, err := Acquire(path, 0o600)
		if err != nil {
			errs <- err

			return
		}

		held <- l
	}()

	// Let the waiter reach flock, so it is already on the old file when the name goes away.
	time.Sleep(100 * time.Millisecond)

	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	var waiter *Lock

	select {
	case waiter = <-held:
	case err := <-errs:
		t.Fatalf("the waiting Acquire: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting Acquire never went through")
	}

	third := make(chan struct{})

	go func() {
		l, err := Acquire(path, 0o600)
		if err != nil {
			errs <- err

			return
		}

		close(third)

		if err := l.Release(); err != nil {
			errs <- err
		}
	}()

	select {
	case <-third:
		t.Fatal("a third Acquire went through, so the waiter holds a file that names nothing")
	case <-time.After(100 * time.Millisecond):
	}

	if err := waiter.Release(); err != nil {
		t.Fatalf("the release of the waiter: %v", err)
	}

	select {
	case <-third:
	case err := <-errs:
		t.Fatalf("the third Acquire: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the third Acquire never went through after the release")
	}
}

func TestTwoPathsDoNotBlockEachOther(t *testing.T) {
	dir := t.TempDir()

	first, err := Acquire(filepath.Join(dir, "a.lock"), 0o600)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	second, err := Acquire(filepath.Join(dir, "b.lock"), 0o600)
	if err != nil {
		t.Fatalf("the lock on the second path: %v", err)
	}

	for _, l := range []*Lock{second, first} {
		if err := l.Release(); err != nil {
			t.Fatalf("Release: %v", err)
		}
	}
}
