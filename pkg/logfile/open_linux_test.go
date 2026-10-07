package logfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A fifo's open stands in for a driver's: an open for a read releases a writer waiting in its own, and an O_PATH handle does not (SHARD-305).
func TestOpenRefusesASpecialFileBeforeItsOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("make the fifo: %v", err)
	}
	released := make(chan error, 1)
	go func() {
		w, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err == nil {
			err = w.Close()
		}
		released <- err
	}()

	// The writer waits within the first few calls, and any call that opens the fifo releases it.
	for range 50 {
		if f, err := Open(path); err == nil {
			t.Fatalf("Open took a fifo: %v", f.Close())
		}
		if err := Truncate(path, 8); err == nil {
			t.Fatal("Truncate took a fifo")
		}
		select {
		case <-released:
			t.Fatal("Open or Truncate opened the fifo before it refused it")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// The control: a plain open does release the writer.
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open the fifo: %v", err)
	}
	defer f.Close()
	select {
	case err := <-released:
		if err != nil {
			t.Fatalf("the writer's open: %v", err)
		}
	case <-time.After(openBudget):
		t.Fatal("an open of the fifo for a read did not release the writer")
	}
}
