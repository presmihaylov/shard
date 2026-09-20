package vzvm_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/ext4"
	"github.com/presmihaylov/shard/services/provider/conformance"
	"github.com/presmihaylov/shard/services/provider/vzvm"
)

const stopGrace = 5 * time.Second

// harness is one provider over one short root: a unix socket path is 104 bytes at most, and t.TempDir is longer.
type harness struct {
	provider    *vzvm.Provider
	root        string
	disk        string
	saveRestore bool

	next atomic.Int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	return newHarnessOn(t, true)
}

// newHarnessOn is a Mac that saves a VM, or one that only pauses it in place.
func newHarnessOn(t *testing.T, saveRestore bool) *harness {
	t.Helper()

	root, err := os.MkdirTemp("", "vz") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	h := &harness{root: root, disk: baseDisk(t, root), saveRestore: saveRestore}
	h.provider = h.open(t)

	return h
}

// open is a daemon start: a provider over the root, which holds nothing of an earlier one in memory.
func (h *harness) open(t *testing.T) *vzvm.Provider {
	t.Helper()

	p, err := vzvm.New(vzvm.Config{
		Shim:        os.Args[0],
		Kernel:      "kernel",
		Init:        initBinary,
		Dir:         h.root,
		Dirs:        h.stateDir,
		SaveRestore: h.saveRestore,
	})
	if err != nil {
		t.Fatalf("open the provider: %v", err)
	}

	return p
}

// stateDir answers for any id, as the repository does; only a spec's directory exists.
func (h *harness) stateDir(id string) (string, error) {
	return filepath.Join(h.root, "s", id), nil
}

func (h *harness) newSpec(t *testing.T, entrypoint ...string) models.SandboxSpec {
	t.Helper()

	id := fmt.Sprintf("sb-%d", h.next.Add(1))
	dir, _ := h.stateDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		// Best effort: a subtest may have stopped and removed this one already, and its errors say nothing new.
		ctx := context.Background()
		h.provider.Stop(ctx, id, stopGrace)
		h.provider.Remove(ctx, id)
	})

	return models.SandboxSpec{ID: id, StateDir: dir, RootDisk: h.disk, Entrypoint: entrypoint, Resources: models.Resources{DiskMiB: 16}}
}

// baseDisk is the smallest ext4 image the clone accepts; the fake guest never mounts it.
func baseDisk(t *testing.T, root string) string {
	t.Helper()

	path := filepath.Join(root, "base.ext4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "etc", Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ext4.Write(&buf, f); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestConformance(t *testing.T) {
	h := newHarness(t)

	conformance.Run(t, conformance.Subject{
		Provider: h.provider,
		NewSpec:  func(t *testing.T) models.SandboxSpec { return h.newSpec(t, "/bin/sh", "-c", "exit 0") },
		NewIgnoresTermSpec: func(t *testing.T) models.SandboxSpec {
			script := fmt.Sprintf("trap '' TERM; echo %s; while true; do sleep 1; done", conformance.ReadyMarker)

			return h.newSpec(t, "/bin/sh", "-c", script)
		},
		SnapshotDir: func(t *testing.T) string { return t.TempDir() },
		Shell:       func(script string) []string { return []string{"/bin/sh", "-c", script} },
		// The fake guest is a host process, so the suite writes under the root; a clone here proves the verbs and not the disk.
		Scratch: h.root,
	})
}

func TestCreateRefusesAnImageWithoutARootDisk(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "exit 0")
	spec.RootDisk = ""

	err := h.provider.Create(t.Context(), spec)
	if err == nil || !strings.Contains(err.Error(), spec.ID) || !strings.Contains(err.Error(), "root disk") {
		t.Fatalf("Create = %v, want a refusal that names the sandbox and the disk", err)
	}
}

// A daemon that starts over a root with a live shim adopts it by its socket, and the sandbox goes on as it was.
func TestANewProviderAdoptsALiveShim(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	again := h.open(t)
	status, err := again.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != models.StateRunning {
		t.Fatalf("the second provider sees %s, want running", status.State)
	}
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	exit, err := again.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "echo adopted"}, Stdout: out})
	written, _ := os.ReadFile(out.Name())
	if err != nil || exit.Code != 0 || strings.TrimSpace(string(written)) != "adopted" {
		t.Fatalf("Exec over the adopted shim = %+v, %q, %v", exit, written, err)
	}
	if err := again.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != models.StateStopped {
		t.Fatalf("the first provider sees %s after the other stopped it, want stopped", status.State)
	}
}

// A pause keeps the save, the disk and the identifier together; a stop of a paused sandbox leaves it stopped and the snapshot whole.
func TestPauseKeepsWhatAResumeAndAForkNeed(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	snap := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"snapshot.json", "vm.vzvmstate", "disk.img", "checkpoint.img"} {
		if _, err := os.Stat(filepath.Join(snap, name)); err != nil {
			t.Errorf("the snapshot lacks %s: %v", name, err)
		}
	}
	if _, err := os.Stat(snap + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the pause left its staging directory: %v", err)
	}
	// The save ends the shim, so the substrate says stopped and the snapshot marker is what says paused.
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after Pause = %+v, %v", status, err)
	}
	// A second Pause is a service retry, and finds nothing left to finish.
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatalf("a second Pause = %v, want a no-op", err)
	}
	if err := h.provider.Pause(t.Context(), spec.ID, t.TempDir()); err == nil || !strings.Contains(err.Error(), "no complete snapshot") {
		t.Fatalf("a second Pause into an empty directory = %v, want a refusal", err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err == nil || !strings.Contains(err.Error(), "resume it first") {
		t.Fatalf("Start of a paused sandbox = %v, want a refusal", err)
	}

	fork := h.newSpec(t)
	if err := h.provider.Fork(t.Context(), snap, fork); err != nil {
		t.Fatal(err)
	}
	status, err = h.provider.Status(t.Context(), fork.ID)
	if err != nil || !status.Alive() {
		t.Fatalf("Status of the fork = %+v, %v", status, err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || !status.Alive() {
		t.Fatalf("Status after Resume = %+v, %v", status, err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err == nil || !strings.Contains(err.Error(), "not paused") {
		t.Fatalf("Resume of a live sandbox = %v, want a refusal", err)
	}

	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after a Stop of a paused sandbox = %+v, %v", status, err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err == nil {
		t.Fatal("Resume after a Stop succeeded, and only a paused sandbox resumes")
	}
}

// A stopped sandbox starts again over the disk the stop kept, and a wait then answers the new run.
func TestStartBootsAgainAfterAStop(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "exit 4")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	exit, err := h.provider.Wait(t.Context(), spec.ID)
	if err != nil || exit.Code != 4 {
		t.Fatalf("Wait = %+v, %v", exit, err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}

	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	exit, err = h.provider.Wait(t.Context(), spec.ID)
	if err != nil || exit.Code != 4 {
		t.Fatalf("Wait after the second Start = %+v, %v", exit, err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err == nil || !strings.Contains(err.Error(), "already runs") {
		t.Fatalf("Start with the entrypoint already run = %v, want a refusal", err)
	}
}

// A host that cannot save has no optional verb: each is refused by name, and none freezes a VM in its shim.
func TestAHostWithoutSaveRefusesTheOptionalVerbs(t *testing.T) {
	h := newHarnessOn(t, false)
	if caps := h.provider.Capabilities(); caps.Pause || caps.Resume || caps.Fork {
		t.Fatalf("Capabilities = %+v, want none", caps)
	}
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	snap := t.TempDir()
	refused := map[string]error{
		models.VerbPause:  h.provider.Pause(t.Context(), spec.ID, snap),
		models.VerbResume: h.provider.Resume(t.Context(), spec.ID, snap),
		models.VerbFork:   h.provider.Fork(t.Context(), snap, h.newSpec(t)),
	}
	for verb, err := range refused {
		var refusal *models.UnsupportedError
		if !errors.As(err, &refusal) || refusal.Verb != verb || refusal.Provider != vzvm.Name {
			t.Errorf("%s = %v, want unsupported on %s", verb, err, vzvm.Name)
		}
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status after the refusals = %+v, %v", status, err)
	}
}

// An exit the loop could not land is an error on every read, not a wait that never ends.
func TestALostExitSurfacesInsteadOfAnEndlessWait(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "exit 3")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(spec.StateDir, 0o700) })
	if err := os.Chmod(spec.StateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := h.provider.Wait(ctx, spec.ID); err == nil || !strings.Contains(err.Error(), "lost its lifecycle state") {
		t.Fatalf("Wait = %v, want the lost exit", err)
	}
	if _, err := h.provider.ExitStatus(t.Context(), spec.ID); err == nil || !strings.Contains(err.Error(), "lost its lifecycle state") {
		t.Fatalf("ExitStatus = %v, want the lost exit", err)
	}
	if _, err := h.provider.Restarts(t.Context(), spec.ID); err == nil || !strings.Contains(err.Error(), "lost its lifecycle state") {
		t.Fatalf("Restarts = %v, want the lost exit", err)
	}
}

// A log that cannot open fails the attach, so no verb reports a sandbox whose output has nowhere to go.
func TestAnAdoptFailsWhenTheLogCannotOpen(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(spec.StateDir, "output.log")
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(log, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := h.open(t).Status(t.Context(), spec.ID); err == nil || !strings.Contains(err.Error(), "open the log") {
		t.Fatalf("Status over a fresh provider = %v, want the log open failure", err)
	}

	// The failed adopt took the guest's one control connection, so the stop goes through a provider that adopts it again.
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if err := h.open(t).Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
}

// A pause that cannot complete its snapshot resumes the VM, keeps the last snapshot and leaves no staging directory.
func TestAFailedPauseResumesTheSandboxAndKeepsTheLastSnapshot(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	snap := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(snap, "vm.vzvmstate"))
	if err != nil {
		t.Fatal(err)
	}

	// Without a disk to copy the snapshot cannot complete, and the pause must give the VM back.
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "disk.img")); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err == nil || !strings.Contains(err.Error(), "copy the disk") {
		t.Fatalf("Pause without a disk = %v, want the copy failure", err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || !status.Alive() {
		t.Fatalf("Status after a failed Pause = %+v, %v; want alive", status, err)
	}
	if _, err := os.Stat(snap + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the failed pause left its staging directory: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(snap, "vm.vzvmstate"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("the last snapshot changed under a failed pause: %v", err)
	}
	if err := h.provider.Pause(t.Context(), spec.ID, t.TempDir()); err == nil || !strings.Contains(err.Error(), "copy the disk") {
		t.Fatalf("a second Pause = %v, want the copy failure again, not an already-paused refusal", err)
	}
}

// A pause that crashed after its record and before its swap leaves the staged snapshot beside the old one; Resume installs the newer and ends the shim.
func TestResumeInstallsTheSnapshotACrashedPauseStaged(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	snap := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}

	// The crash state by hand: a complete second snapshot in the staging directory, a record that says paused, and the shim still up.
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	staged := snap + ".tmp"
	if err := os.CopyFS(staged, os.DirFS(snap)); err != nil {
		t.Fatal(err)
	}
	setJSON(t, filepath.Join(staged, "snapshot.json"), "pause", 2)
	setJSON(t, filepath.Join(dir, "vm.json"), "paused", true)
	setJSON(t, filepath.Join(dir, "vm.json"), "pauses", 2)

	// The service retries the pause, which finishes the crashed one: the staged snapshot goes in and the shim goes.
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	if got := readJSON(t, filepath.Join(snap, "snapshot.json"))["pause"]; got != 2.0 {
		t.Fatalf("the snapshot in place is pause %v, want 2, the staged one", got)
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging directory is still there: %v", err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after the finishing Pause = %+v, %v; want stopped", status, err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || !status.Alive() {
		t.Fatalf("Status after Resume = %+v, %v; want alive", status, err)
	}

	// An older staged snapshot, left by a swap whose cleanup failed, goes, and the one in place stays.
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(staged, os.DirFS(snap)); err != nil {
		t.Fatal(err)
	}
	setJSON(t, filepath.Join(staged, "snapshot.json"), "pause", 1)
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	if got := readJSON(t, filepath.Join(snap, "snapshot.json"))["pause"]; got != 3.0 {
		t.Fatalf("the snapshot in place is pause %v, want 3", got)
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale staging directory is still there: %v", err)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatal(err)
	}

	return m
}

func setJSON(t *testing.T, path, key string, value any) {
	t.Helper()
	m := readJSON(t, path)
	m[key] = value
	blob, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
}
