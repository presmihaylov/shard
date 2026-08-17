package store

import (
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
