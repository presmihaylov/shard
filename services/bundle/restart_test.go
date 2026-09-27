package bundle_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/bundle"
)

// countBudget bounds a read that must answer, so a file that blocks fails rather than hangs.
const countBudget = 5 * time.Second

// heapBudget is far under the file the cap case plants, so a read that is not bounded shows.
const heapBudget = 1 << 20

// The guest writes /.shard, and the daemon reads the count from it every second, as root on the host.
func TestRestartCountReadsOnlyASmallRegularFile(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string) string{
		"a symbolic link to /dev/zero": func(t *testing.T, dir string) string {
			return symlink(t, "/dev/zero", filepath.Join(dir, "restarts.json"))
		},
		"a symbolic link to a host file": func(t *testing.T, dir string) string {
			host := filepath.Join(t.TempDir(), "host.json")
			write(t, host, `{"count":7}`)

			return symlink(t, host, filepath.Join(dir, "restarts.json"))
		},
		"a fifo": func(t *testing.T, dir string) string {
			path := filepath.Join(dir, "restarts.json")
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatalf("make the fifo: %v", err)
			}

			return path
		},
		"a device": func(*testing.T, string) string {
			return "/dev/zero"
		},
		"a file far over the cap": func(t *testing.T, dir string) string {
			path := filepath.Join(dir, "restarts.json")
			write(t, path, `{"count":7}`+strings.Repeat(" ", 8<<20))

			return path
		},
	}

	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			path := plant(t, t.TempDir())

			type answer struct {
				err       error
				allocated uint64
			}
			answered := make(chan answer, 1)
			go func() {
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				_, err := bundle.Bundle{RestartFile: path}.RestartCount()
				runtime.ReadMemStats(&after)
				answered <- answer{err: err, allocated: after.TotalAlloc - before.TotalAlloc}
			}()

			select {
			case got := <-answered:
				if got.err == nil {
					t.Fatalf("RestartCount read %s", name)
				}
				if !strings.Contains(got.err.Error(), path) {
					t.Errorf("the refusal is %q, and it must name the file", got.err)
				}
				if got.allocated > heapBudget {
					t.Errorf("RestartCount allocated %d bytes to refuse %s, want under %d", got.allocated, name, heapBudget)
				}
			case <-time.After(countBudget):
				t.Fatalf("RestartCount did not answer within %s for %s", countBudget, name)
			}
		})
	}
}

func symlink(t *testing.T, target, path string) string {
	t.Helper()

	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("link %s to %s: %v", path, target, err)
	}

	return path
}
