package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
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

// SHARD-368: a pause the last daemon did not finish leaves a snapshot .tmp with no record, so the start sweeps it.
func TestReconcileSweepsTheUnfinishedSnapshotTheLastDaemonLeft(t *testing.T) {
	root := t.TempDir()
	left := filepath.Join(root, "snapshots", "quiet-otter-0000.tmp")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatalf("plant the unfinished snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(left, "checkpoint.img"), []byte("x"), 0o600); err != nil {
		t.Fatalf("plant the checkpoint: %v", err)
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
	if !slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, "swept 1 orphan snapshot") }) {
		t.Errorf("the reconcile reported %q, want the snapshot sweep in it", lines)
	}
}

// stagingProvider is a substrate a start reconciles: its sandbox is gone, and it drops or keeps the staging a cut pause left.
type stagingProvider struct {
	models.Provider

	keeps bool
}

func (p stagingProvider) Name() string { return "fake" }

func (p stagingProvider) Status(context.Context, string) (models.Status, error) {
	return models.Status{}, nil
}

func (p stagingProvider) AdoptStaging(dir string) error {
	if p.keeps {
		return nil
	}

	return os.RemoveAll(dir + ".tmp")
}

// SHARD-428: a start says once what happened to the staging a cut pause left, and never calls kept one the provider removes.
func TestReconcileSaysOnceWhatHappenedToTheStagingOfACutPause(t *testing.T) {
	for _, tc := range []struct {
		name  string
		keeps bool
		want  string
	}{
		{name: "a provider that never reads it", want: "removed"},
		{name: "a provider that finishes it at a resume", keeps: true, want: "kept"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &deps{cfg: Config{Root: t.TempDir()}}
			repo, err := d.repo()
			if err != nil {
				t.Fatalf("open the repository: %v", err)
			}
			sb, err := repo.Create(models.Sandbox{State: models.StateRunning, PID: 42, Pausing: true})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			dir, err := repo.SnapshotDir(sb.ID)
			if err != nil {
				t.Fatalf("SnapshotDir: %v", err)
			}
			staging := dir + ".tmp"
			if err := os.MkdirAll(staging, 0o700); err != nil {
				t.Fatalf("plant the staging: %v", err)
			}

			provider := stagingProvider{keeps: tc.keeps}
			d.providerSvc = provider
			l := &lifecycle{deps: d, base: t.Context(), svc: sandbox.New(sandbox.Config{Repo: repo, Provider: provider})}
			r := reconciler{deps: d, lifecycle: l}

			var lines []string
			if err := r.Reconcile(t.Context(), func(line string) { lines = append(lines, line) }); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			named := slices.DeleteFunc(slices.Clone(lines), func(line string) bool { return !strings.Contains(line, staging) })
			if len(named) != 1 || !strings.Contains(named[0], tc.want) {
				t.Errorf("the start named the staging in %q, want one line that says %s", named, tc.want)
			}
			if !tc.keeps && slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, "kept") }) {
				t.Errorf("the start reported %q, want no line that calls kept the staging it removed", lines)
			}
		})
	}
}
