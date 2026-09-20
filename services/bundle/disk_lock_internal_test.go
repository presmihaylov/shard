package bundle

import (
	"sync"
	"testing"
	"time"
)

// TestTwoWithDiskCallsOnOneImageRunOneAtATime pins SHARD-251: two clones of one stopped source must not race its mount.
func TestTwoWithDiskCallsOnOneImageRunOneAtATime(t *testing.T) {
	b := Bundle{Image: t.TempDir() + "/disk.img"}

	var mu sync.Mutex
	inside, overlap := 0, false
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			err := b.withDisk(func() error {
				mu.Lock()
				inside++
				overlap = overlap || inside > 1
				mu.Unlock()
				time.Sleep(20 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()

				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	if overlap {
		t.Fatal("two withDisk calls on one image ran at the same time")
	}
	if n := heldDiskLocks(); n != 0 {
		t.Fatalf("%d disk locks left in the table after every call returned, want none", n)
	}
}

func heldDiskLocks() int {
	diskLocks.Lock()
	defer diskLocks.Unlock()

	return len(diskLocks.byImage)
}

func TestWithDiskOnTwoImagesDoesNotSerialise(t *testing.T) {
	dir := t.TempDir()
	a, b := Bundle{Image: dir + "/a.img"}, Bundle{Image: dir + "/b.img"}

	release := a.lockDisk()
	defer func() {
		release()
		if n := heldDiskLocks(); n != 0 {
			t.Errorf("%d disk locks left in the table after the release, want none", n)
		}
	}()

	done := make(chan error, 1)
	go func() { done <- b.withDisk(func() error { return nil }) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("a held lock on one image blocked withDisk on another")
	}
}
