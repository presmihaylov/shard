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

	held := make(chan struct{})

	go func() {
		second, err := Acquire(path, 0o600)
		if err != nil {
			t.Errorf("the second Acquire: %v", err)

			return
		}

		close(held)

		if err := second.Release(); err != nil {
			t.Errorf("Release: %v", err)
		}
	}()

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

func TestSharedLocksAreHeldTogether(t *testing.T) {
	path := lockPath(t)

	first, err := AcquireShared(path, 0o600)
	if err != nil {
		t.Fatalf("AcquireShared: %v", err)
	}

	second, err := AcquireShared(path, 0o600)
	if err != nil {
		t.Fatalf("the second AcquireShared: %v", err)
	}

	if err := second.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestTheExclusiveLockBlocksAReader(t *testing.T) {
	path := lockPath(t)

	writer, err := Acquire(path, 0o600)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	held := make(chan struct{})

	go func() {
		reader, err := AcquireShared(path, 0o600)
		if err != nil {
			t.Errorf("AcquireShared: %v", err)

			return
		}

		close(held)

		if err := reader.Release(); err != nil {
			t.Errorf("Release: %v", err)
		}
	}()

	select {
	case <-held:
		t.Fatal("a reader went through while a writer held the lock")
	case <-time.After(50 * time.Millisecond):
	}

	if err := writer.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the reader never went through after the release")
	}
}
