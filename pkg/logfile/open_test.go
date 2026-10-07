package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// openBudget bounds an open that must answer, so a file that blocks fails rather than hangs.
const openBudget = 5 * time.Second

// Guest root writes in the log directory, and the daemon reads and truncates there as host root.
func TestOpenAndTruncateRefuseALinkOrASpecialFile(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string) string{
		"a symbolic link to a host file": func(t *testing.T, dir string) string {
			host := filepath.Join(t.TempDir(), "host")
			if err := os.WriteFile(host, []byte(strings.Repeat("h", 64)), 0o600); err != nil {
				t.Fatalf("write the host file: %v", err)
			}
			path := filepath.Join(dir, "app.log")
			if err := os.Symlink(host, path); err != nil {
				t.Fatalf("plant the link: %v", err)
			}

			return path
		},
		"a fifo": func(t *testing.T, dir string) string {
			path := filepath.Join(dir, "app.log")
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatalf("make the fifo: %v", err)
			}

			return path
		},
		"a directory": func(t *testing.T, dir string) string {
			path := filepath.Join(dir, "app.log")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatalf("make the directory: %v", err)
			}

			return path
		},
		"a device": func(*testing.T, string) string { return "/dev/zero" },
	}

	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			path := plant(t, t.TempDir())
			type answer struct {
				opened    bool
				truncated error
			}
			answered := make(chan answer, 1)
			go func() {
				f, err := Open(path)
				if err == nil {
					defer f.Close()
				}
				answered <- answer{opened: err == nil, truncated: Truncate(path, 8)}
			}()

			select {
			case got := <-answered:
				if got.opened {
					t.Errorf("Open took %s", name)
				}
				if got.truncated == nil {
					t.Errorf("Truncate took %s", name)
				}
			case <-time.After(openBudget):
				t.Fatalf("Open or Truncate did not answer within %s for %s", openBudget, name)
			}
		})
	}
}

func TestTruncateLeavesTheFilePastALinkAlone(t *testing.T) {
	host := filepath.Join(t.TempDir(), "host")
	want := strings.Repeat("h", 64)
	if err := os.WriteFile(host, []byte(want), 0o600); err != nil {
		t.Fatalf("write the host file: %v", err)
	}
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.Symlink(host, path); err != nil {
		t.Fatalf("plant the link: %v", err)
	}

	if err := Truncate(path, 8); err == nil {
		t.Fatal("Truncate followed the link")
	}
	got, err := os.ReadFile(host)
	if err != nil || string(got) != want {
		t.Errorf("the host file holds %q, %v, want it untouched", got, err)
	}
}

func TestOpenReadsARegularLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("line\n"), 0o600); err != nil {
		t.Fatalf("write the log: %v", err)
	}

	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	got := make([]byte, 16)
	n, err := f.Read(got)
	if err != nil || string(got[:n]) != "line\n" {
		t.Errorf("read %q, %v, want %q", got[:n], err, "line\n")
	}
}
