package daemon

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

type fakeHeldLogs map[string][]string

func (f fakeHeldLogs) HeldLogs(id string) ([]string, error) { return f[id], nil }

// The first sandbox names a directory, which no truncate opens, and the rest are still bounded.
func TestTruncateHeldLogsBoundsOnlyALogPastMax(t *testing.T) {
	dir := t.TempDir()
	big, small := filepath.Join(dir, "big.log"), filepath.Join(dir, "small.log")
	if err := os.WriteFile(big, bytes.Repeat([]byte("a"), 11), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(small, bytes.Repeat([]byte("b"), 10), 0o600); err != nil {
		t.Fatal(err)
	}

	sandboxes := []models.Sandbox{{ID: "broken"}, {ID: "one"}, {ID: "two"}, {ID: "gone"}}
	provider := fakeHeldLogs{"broken": {dir}, "one": {big}, "two": {small}, "gone": {filepath.Join(dir, "gone.log")}}
	err := truncateHeldLogs(provider, sandboxes, 10)
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("truncateHeldLogs returned %v, want the broken sandbox named", err)
	}

	for path, want := range map[string]int64{big: 0, big + ".1": 10, small: 10} {
		info, err := os.Stat(path)
		if err != nil || info.Size() != want {
			t.Errorf("%s: %v, want %d bytes", filepath.Base(path), err, want)
		}
	}
	if _, err := os.Stat(small + ".1"); !os.IsNotExist(err) {
		t.Errorf("small.log.1 exists: %v", err)
	}
}

// fakeOutputLogs is a substrate whose daemon writes the output log itself; it bounds every id but the one named to fail.
type fakeOutputLogs struct {
	models.Provider

	fail    string
	bounded []string
}

func (f *fakeOutputLogs) BoundOutputLog(id string, _ int64) error {
	if id == f.fail {
		return errors.New("read-only file system")
	}
	f.bounded = append(f.bounded, id)

	return nil
}

// One output log it cannot bound leaves the others bounded, and a substrate that writes none is left alone.
func TestBoundOutputLogsBoundsEverySandboxBeforeItReports(t *testing.T) {
	sandboxes := []models.Sandbox{{ID: "one"}, {ID: "broken"}, {ID: "two"}}
	provider := &fakeOutputLogs{fail: "broken"}

	err := boundOutputLogs(provider, sandboxes, 10)
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("boundOutputLogs returned %v, want the broken sandbox named", err)
	}
	if !slices.Equal(provider.bounded, []string{"one", "two"}) {
		t.Errorf("bounded %v, want one and two", provider.bounded)
	}

	if err := boundOutputLogs(struct{ models.Provider }{}, sandboxes, 10); err != nil {
		t.Errorf("boundOutputLogs on a substrate with no output log: %v", err)
	}
}
