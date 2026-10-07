package bundle_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/services/bundle"
)

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
