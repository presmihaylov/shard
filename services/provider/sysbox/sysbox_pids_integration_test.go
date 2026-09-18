//go:build integration

package sysbox_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/services/bundle"
)

// pidsBound is small so the fork storm hits it fast and the sandbox never holds many host processes.
const pidsBound = 64

// TestEverySandboxGetsADefaultPidsBound is the first half of SHARD-171: a sandbox that names no bound
// still runs under DefaultPidsMax, so an unbounded fork bomb cannot exhaust host PIDs.
func TestEverySandboxGetsADefaultPidsBound(t *testing.T) {
	h := newHarness(t)

	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	dir := filepath.Join(cgroup.Root, bundle.CgroupsPath(spec.ID))
	want := strconv.Itoa(bundle.DefaultPidsMax)
	if got := strings.TrimSpace(readFile(t, filepath.Join(dir, "pids.max"))); got != want {
		t.Errorf("pids.max is %s, want the default %s", got, want)
	}
}

// TestAPidsBoundStopsAForkStorm is the SHARD-171 acceptance: a fork storm inside a bounded sandbox is
// denied at the bound. pids.events counts every fork the controller refused, so a positive count is
// the kernel's own proof that the bound stopped the storm, and the host stayed whole.
func TestAPidsBoundStopsAForkStorm(t *testing.T) {
	h := newHarness(t)

	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	spec.Resources = models.Resources{PidsMax: pidsBound}

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	dir := filepath.Join(cgroup.Root, bundle.CgroupsPath(spec.ID))
	if got := strings.TrimSpace(readFile(t, filepath.Join(dir, "pids.max"))); got != strconv.Itoa(pidsBound) {
		t.Fatalf("pids.max is %s, want %d", got, pidsBound)
	}

	// Each sleep runs long enough to hold its slot, so the storm reaches the bound before any of them exits.
	storm := "i=0; while [ $i -lt 400 ]; do sleep 30 & i=$((i+1)); done"
	if _, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{
		Argv:   []string{"/bin/sh", "-c", storm},
		Report: func(int) {},
	}); err != nil {
		t.Fatalf("Exec the fork storm: %v", err)
	}

	if denied := pidsMaxEvents(t, filepath.Join(dir, "pids.events")); denied == 0 {
		t.Errorf("pids.events counted no denied forks, so the bound never stopped the storm")
	}
}

// TestAPreFixConfigStillGetsThePidsBoundOnRestart is the SHARD-171 residual: a sandbox whose config.json
// predates the pids bound must still be capped when it runs again. boundPids writes pids.max on the host
// cgroup on every launch, so a restart over a config.json with no Pids section is bounded all the same.
func TestAPreFixConfigStillGetsThePidsBoundOnRestart(t *testing.T) {
	h := newHarness(t)

	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Make the bundle look like one written before SHARD-171: runc then builds the cgroup with no pids
	// bound, so only boundPids caps the restart.
	stripPids(t, spec.StateDir)

	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("restart over the pre-fix config: %v", err)
	}

	dir := filepath.Join(cgroup.Root, bundle.CgroupsPath(spec.ID))
	want := strconv.Itoa(bundle.DefaultPidsMax)
	if got := strings.TrimSpace(readFile(t, filepath.Join(dir, "pids.max"))); got != want {
		t.Errorf("pids.max is %s after a restart over a pre-fix config, want the default %s", got, want)
	}
}

// stripPids removes the Pids section from a bundle's config.json, so the sandbox looks like one built
// before SHARD-171 wrote a pids bound into config.json.
func stripPids(t *testing.T, stateDir string) {
	t.Helper()

	b, err := bundle.Open(stateDir)
	if err != nil {
		t.Fatalf("open the bundle: %v", err)
	}

	path := filepath.Join(b.Dir, "config.json")

	var spec specs.Spec
	if err := json.Unmarshal([]byte(readFile(t, path)), &spec); err != nil {
		t.Fatalf("parse config.json: %v", err)
	}
	spec.Linux.Resources.Pids = nil

	blob, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("re-marshal config.json: %v", err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
}

// pidsMaxEvents reads the "max" counter from pids.events: the times a fork failed because pids.max was hit.
func pidsMaxEvents(t *testing.T, path string) int {
	t.Helper()

	for line := range strings.SplitSeq(readFile(t, path), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || key != "max" {
			continue
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("parse the max counter %q in %s: %v", value, path, err)
		}

		return n
	}

	t.Fatalf("no max counter in %s", path)
	return 0
}
