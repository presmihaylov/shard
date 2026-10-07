package bundle_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/presmihaylov/shard/services/bundle"
)

func TestProcessLogRefusesANameThatIsNoValidOne(t *testing.T) {
	b, err := bundle.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for _, name := range []string{"", "../web", "Web", "web/log", "a b"} {
		if path, err := b.ProcessLog(name); err == nil {
			t.Errorf("ProcessLog(%q) = %s, want a refusal", name, path)
		}
	}
	path, err := b.ProcessLog("web")
	if err != nil || path != filepath.Join(b.Logs, "web.log") {
		t.Errorf("ProcessLog(web) = %s, %v, want %s", path, err, filepath.Join(b.Logs, "web.log"))
	}
}

// The logs dir is guest-writable, so only a regular file under a valid name is a log to bound.
func TestProcessLogsNamesOnlyTheLogsShardInitOpens(t *testing.T) {
	b, err := bundle.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if logs, err := b.ProcessLogs(); err != nil || logs != nil {
		t.Fatalf("ProcessLogs with no disk up = %v, %v, want none", logs, err)
	}

	if err := os.MkdirAll(filepath.Join(b.Logs, "dir.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"web.log", "worker.log", "Web.log", "notes.txt", ".log", "web.log.1"} {
		if err := os.WriteFile(filepath.Join(b.Logs, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(b.Logs, "link.log")); err != nil {
		t.Fatal(err)
	}

	logs, err := b.ProcessLogs()
	if err != nil {
		t.Fatalf("ProcessLogs: %v", err)
	}
	slices.Sort(logs)
	want := []string{filepath.Join(b.Logs, "web.log"), filepath.Join(b.Logs, "worker.log")}
	if !slices.Equal(logs, want) {
		t.Errorf("ProcessLogs = %v, want %v", logs, want)
	}
}

// A guest that floods the dir costs one bounded read, and only its own logs go unrotated.
func TestProcessLogsReadsABoundedListing(t *testing.T) {
	b, err := bundle.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := os.MkdirAll(b.Logs, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 1100 {
		if err := os.WriteFile(filepath.Join(b.Logs, fmt.Sprintf("p%d.log", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	logs, err := b.ProcessLogs()
	if err != nil {
		t.Fatalf("ProcessLogs: %v", err)
	}
	if len(logs) != 1024 {
		t.Errorf("ProcessLogs named %d logs, want the 1024 one read holds", len(logs))
	}
}
