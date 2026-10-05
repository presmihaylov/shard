package bundle_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

// countBudget bounds a read that must answer, so a file that blocks fails rather than hangs.
const countBudget = 5 * time.Second

// heapBudget is far under the file the cap case plants, so a read that is not bounded shows.
const heapBudget = 1 << 20

// The daemon reads a VM count every second, as root on the host, so it takes only a small regular file.
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
				_, err := bundle.ReadRestartCount(path)
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

// The count rides the exit record, so nothing under /.shard feeds it (SHARD-634).
func TestRestartCountReadsTheExitRecord(t *testing.T) {
	b := bundle.Bundle{ExitFile: filepath.Join(t.TempDir(), "exit.json")}

	count, err := b.RestartCount()
	if err != nil || count != (models.RestartCount{}) {
		t.Fatalf("RestartCount() = %+v, %v with no record, want zero and no error", count, err)
	}

	want := models.RestartCount{Count: 2, GaveUp: true, Ended: true}
	if err := bundle.WriteExitReport(b.ExitFile, models.ExitReport{Kind: models.ExitReportKind, Code: 1, Restarts: want}); err != nil {
		t.Fatalf("write the exit record: %v", err)
	}
	count, err = b.RestartCount()
	if err != nil || count != want {
		t.Errorf("RestartCount() = %+v, %v, want %+v off the exit record", count, err, want)
	}

	write(t, b.ExitFile, strings.Repeat(" ", 8<<10))
	if _, err := b.RestartCount(); !errors.Is(err, models.ErrExitFileTooLarge) {
		t.Errorf("RestartCount over an oversize exit file answered %v, want ErrExitFileTooLarge", err)
	}
}

// A start must never fail on what an earlier run left at a supervisor file, whatever its type (SHARD-635).
func TestClearRunRemovesATreeAndFollowsNoLink(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(t.TempDir(), "host.json")
	write(t, host, "{}")
	b := bundle.Bundle{
		ExitFile:    filepath.Join(dir, "exit.json"),
		ReadyFile:   filepath.Join(dir, "started"),
		ChangedFile: filepath.Join(dir, "spec-changed"),
	}
	write(t, b.ExitFile, `{"code":0}`)
	if err := os.MkdirAll(filepath.Join(b.ReadyFile, "x"), 0o700); err != nil {
		t.Fatalf("plant the tree: %v", err)
	}
	symlink(t, host, b.ChangedFile)

	if err := b.ClearRun(); err != nil {
		t.Fatalf("ClearRun: %v", err)
	}

	for _, path := range []string{b.ExitFile, b.ReadyFile, b.ChangedFile} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there after ClearRun: %v", path, err)
		}
	}
	if _, err := os.Stat(host); err != nil {
		t.Errorf("ClearRun reached the file past the link: %v", err)
	}
	if err := b.ClearRun(); err != nil {
		t.Errorf("ClearRun over nothing: %v", err)
	}
}

func symlink(t *testing.T, target, path string) string {
	t.Helper()

	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("link %s to %s: %v", path, target, err)
	}

	return path
}
