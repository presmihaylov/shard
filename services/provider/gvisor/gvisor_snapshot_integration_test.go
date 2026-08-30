//go:build integration

package gvisor_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// A pause is a checkpoint and a delete, so a resumed sandbox is a new runsc container that must still
// be the same run: same files, same entrypoint mid-loop, and its output still reaching the log.
func TestAPausedSandboxResumesWhereItWas(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/sh", "-c", "i=0; while true; do i=$((i+1)); echo tick $i; sleep 0.2; done")
	snapshot := filepath.Join(t.TempDir(), "snapshot")

	// /root is on the writable layer, and the gofer must flush it to the host before the checkpoint.
	execIn(t, h, spec.ID, "touch /root/marker")

	started := time.Now()
	if err := h.provider.Pause(t.Context(), spec.ID, snapshot); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	t.Logf("pause took %s", time.Since(started))

	if _, err := os.Stat(filepath.Join(snapshot, "checkpoint.img")); err != nil {
		t.Errorf("the pause wrote no checkpoint: %v", err)
	}
	assertAlive(t, h, spec.ID, false)
	assertMounted(t, h, spec.ID, false)

	path, err := h.provider.LogPath(spec.ID)
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}
	ticksAtPause := strings.Count(readFile(t, path), "tick")
	if ticksAtPause == 0 {
		t.Fatal("the entrypoint never ticked before the pause")
	}

	started = time.Now()
	if err := h.provider.Resume(t.Context(), spec.ID, snapshot); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	t.Logf("resume took %s", time.Since(started))

	assertAlive(t, h, spec.ID, true)
	assertMounted(t, h, spec.ID, true)

	if got := execIn(t, h, spec.ID, "ls /root"); !strings.Contains(got, "marker") {
		t.Errorf("the resumed sandbox has %q in /root, want the marker the paused run wrote", got)
	}

	// The tick counter lives in the sandbox's memory, so a resumed loop counts on from where it froze.
	time.Sleep(time.Second)
	log := readFile(t, path)
	if got := strings.Count(log, "tick"); got <= ticksAtPause {
		t.Errorf("the log holds %d ticks after the resume, want more than the %d it held at the pause", got, ticksAtPause)
	}
	if !strings.Contains(log, "tick 1\n") {
		t.Errorf("the log lost the first run's output: %q", log)
	}
}

// A resume does not consume the snapshot: the same one brings the sandbox back as often as asked.
func TestASnapshotSurvivesItsResume(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/sh", "-c", "sleep 300")
	snapshot := filepath.Join(t.TempDir(), "snapshot")

	if err := h.provider.Pause(t.Context(), spec.ID, snapshot); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	for round := range 2 {
		if err := h.provider.Resume(t.Context(), spec.ID, snapshot); err != nil {
			t.Fatalf("Resume %d: %v", round, err)
		}
		assertAlive(t, h, spec.ID, true)

		if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
			t.Fatalf("Stop %d: %v", round, err)
		}
	}
}

func TestPauseAndResumeRefuseTheWrongState(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/sh", "-c", "sleep 300")
	snapshot := filepath.Join(t.TempDir(), "snapshot")

	// A running sandbox is one no pause deleted, so a restore over it is refused before runsc sees it.
	if err := os.MkdirAll(snapshot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, snapshot); err == nil || !strings.Contains(err.Error(), "running") {
		t.Errorf("Resume of a running sandbox returned %v, want a refusal that names the state", err)
	}
	if err := os.RemoveAll(snapshot); err != nil {
		t.Fatal(err)
	}
	assertAlive(t, h, spec.ID, true)

	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if err := h.provider.Pause(t.Context(), spec.ID, snapshot); err == nil {
		t.Error("Pause of a stopped sandbox went through")
	}
	if _, err := os.Stat(snapshot); err == nil {
		t.Error("the refused pause made the snapshot directory")
	}
}

// The snapshot holds the guest's memory, so it must go with the sandbox and never with a stop.
func TestAStopAfterAPauseKeepsTheSnapshot(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/sh", "-c", "sleep 300")
	snapshot := filepath.Join(t.TempDir(), "snapshot")

	if err := h.provider.Pause(t.Context(), spec.ID, snapshot); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop of a paused sandbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapshot, "checkpoint.img")); err != nil {
		t.Errorf("the stop took the snapshot: %v", err)
	}

	// The stop left the record's run over, and start is the verb that runs a stopped sandbox again.
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start after a paused stop: %v", err)
	}
	assertAlive(t, h, spec.ID, true)
}

func execIn(t *testing.T, h *harness, id, script string) string {
	t.Helper()

	out, err := os.CreateTemp(t.TempDir(), "exec")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	exit, err := h.provider.Exec(t.Context(), id, models.ExecSpec{Argv: []string{"/bin/sh", "-c", script}, Stdout: out, Stderr: out})
	if err != nil {
		t.Fatalf("Exec %q: %v", script, err)
	}
	got := readFile(t, out.Name())
	if exit.Code != 0 {
		t.Fatalf("Exec %q exited %d: %s", script, exit.Code, got)
	}

	return got
}
