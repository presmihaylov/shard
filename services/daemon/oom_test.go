package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// The tick builds the substrate only once a record says running, so a host without runsc keeps its daemon.
func TestOOMRestartAsksForNoSubstrateWhileNothingRuns(t *testing.T) {
	d := noRunscDeps(t)
	if _, err := d.repoSvc.Create(models.Sandbox{Image: "alpine", State: models.StateStopped}); err != nil {
		t.Fatalf("create the record: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	task := oomRestart{deps: d, lifecycle: &lifecycle{deps: d}, interval: time.Millisecond}
	if err := task.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want a quiet end over a root with nothing running", err)
	}
}

func TestOOMRestartFailsLoudWhenTheSubstrateIsGone(t *testing.T) {
	d := noRunscDeps(t)
	if _, err := d.repoSvc.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 42}); err != nil {
		t.Fatalf("create the record: %v", err)
	}

	_, want := d.lifecycle()
	if want == nil {
		t.Skip("this host holds a substrate")
	}

	task := oomRestart{deps: d, lifecycle: &lifecycle{deps: d}, interval: time.Millisecond}
	err := task.Run(t.Context())
	if err == nil || err.Error() != want.Error() {
		t.Fatalf("Run = %v, want the layers' own refusal %v, so the supervisor logs it", err, want)
	}
}

// noRunscDeps is a gvisor daemon's layers on a host where runsc is not on PATH, with its repository built.
func noRunscDeps(t *testing.T) *deps {
	t.Helper()

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "runsc"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write the fake runsc: %v", err)
	}
	t.Setenv("PATH", bin)

	d := &deps{cfg: Config{Root: t.TempDir(), Out: os.Stderr, InitPath: "/usr/local/bin/shard-init", Provider: "gvisor"}}
	if _, err := d.repo(); err != nil {
		t.Fatalf("build the repository: %v", err)
	}

	return d
}
