package vzvm_test

import (
	"context"
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
	w := ext4.NewWriter(f)
	if err := w.Create("etc", &ext4.File{Mode: ext4.S_IFDIR | 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
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
	for _, name := range []string{"snapshot.json", "vm.vzvmstate", "disk.img"} {
		if _, err := os.Stat(filepath.Join(snap, name)); err != nil {
			t.Errorf("the snapshot lacks %s: %v", name, err)
		}
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StatePaused {
		t.Fatalf("Status after Pause = %+v, %v", status, err)
	}
	if err := h.provider.Pause(t.Context(), spec.ID, t.TempDir()); err == nil || !strings.Contains(err.Error(), "already paused") {
		t.Fatalf("a second Pause = %v, want a refusal", err)
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

// A host that cannot save still pauses: the VM freezes in its shim, and a fork is refused as unsupported.
func TestAHostWithoutSaveFreezesTheVMInPlace(t *testing.T) {
	h := newHarnessOn(t, false)
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
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StatePaused {
		t.Fatalf("Status after Pause = %+v, %v", status, err)
	}
	if _, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "exit 0"}}); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("Exec on a paused sandbox = %v, want a refusal", err)
	}
	var refusal *models.UnsupportedError
	if err := h.provider.Fork(t.Context(), snap, h.newSpec(t)); !errors.As(err, &refusal) || refusal.Verb != models.VerbFork {
		t.Fatalf("Fork = %v, want unsupported", err)
	}

	if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status after Resume = %+v, %v", status, err)
	}
	if exit, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "exit 0"}}); err != nil || exit.Code != 0 {
		t.Fatalf("Exec after Resume = %+v, %v", exit, err)
	}

	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
	if time.Since(began) >= stopGrace {
		t.Fatal("Stop of a frozen sandbox waited the grace, and nothing in it runs to owe one to")
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after Stop = %+v, %v", status, err)
	}
}
