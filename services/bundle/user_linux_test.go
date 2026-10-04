package bundle_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/presmihaylov/shard/services/bundle"
)

// A fifo's open stands in for a driver's: an open for a read releases a writer waiting in its own, and an O_PATH handle does not (SHARD-548).
func TestResolveUserRefusesASpecialFileBeforeItsOpen(t *testing.T) {
	for name, passwd := range map[string]string{"passwd": "", "group": "root:x:0:0:root:/root:/bin/sh\n"} {
		t.Run(name, func(t *testing.T) {
			rootfs := rootFSWith(t, passwd, "")
			path := filepath.Join(rootfs, "etc", name)
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
				if _, err := bundle.ResolveUser(rootfs, "root"); err == nil {
					t.Fatalf("ResolveUser read a %s that is a fifo", name)
				}
				select {
				case <-released:
					t.Fatalf("ResolveUser opened the %s fifo for a read before it refused it", name)
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
			case <-time.After(resolveBudget):
				t.Fatal("an open of the fifo for a read did not release the writer")
			}
		})
	}
}
