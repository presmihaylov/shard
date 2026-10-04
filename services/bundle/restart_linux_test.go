package bundle_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

// A fifo's open stands in for a driver's: an open for a read releases a writer waiting in its own, and an O_PATH handle does not (SHARD-305).
func TestRestartCountRefusesASpecialFileBeforeItsOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restarts.json")
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

	// The writer waits within the first few calls, and any call that opens the fifo for a read releases it.
	for range 50 {
		if _, err := (bundle.Bundle{RestartFile: path}).RestartCount(); err == nil {
			t.Fatal("RestartCount read a fifo")
		}
		select {
		case <-released:
			t.Fatal("RestartCount opened the fifo for a read before it refused it")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// The control: the open the fix replaced does release the writer.
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
	case <-time.After(countBudget):
		t.Fatal("an open of the fifo for a read did not release the writer")
	}
}

// The guest's /.shard is only reachable on Linux, where the O_PATH open takes a link itself rather than failing on it.
func TestRestartCountNamesALinkTheGuestForged(t *testing.T) {
	path := symlink(t, "/etc/hostname", filepath.Join(t.TempDir(), "restarts.json"))

	if _, err := (bundle.Bundle{RestartFile: path}).RestartCount(); !errors.Is(err, models.ErrRestartFileForged) {
		t.Errorf("RestartCount over a link answered %v, want ErrRestartFileForged", err)
	}
}
