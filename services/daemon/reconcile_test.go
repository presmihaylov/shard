package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// SHARD-158: the exec scratch of the last daemon is nobody's once the lock changes hands, so the start sweeps it.
func TestReconcileSweepsTheExecScratchTheLastDaemonLeft(t *testing.T) {
	root := t.TempDir()
	left := filepath.Join(root, execDir, "shard-exec-1a2b")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatalf("plant the scratch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(left, "pid"), []byte("4242\n"), 0o600); err != nil {
		t.Fatalf("plant the pid file: %v", err)
	}

	d := &deps{cfg: Config{Root: root}}
	r := reconciler{deps: d, lifecycle: &lifecycle{deps: d, base: t.Context()}}

	var lines []string
	if err := r.Reconcile(t.Context(), func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s after the sweep: %v, want it gone", left, err)
	}
	if !slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, "swept 1 exec") }) {
		t.Errorf("the reconcile reported %q, want the sweep in it", lines)
	}

	// A root with nothing to sweep says nothing, so a quiet start stays quiet.
	lines = nil
	if err := r.Reconcile(t.Context(), func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatalf("Reconcile over a swept root: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("the second reconcile reported %q, want nothing", lines)
	}
}
