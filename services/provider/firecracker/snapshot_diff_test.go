package firecracker_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/provider/firecracker"
)

// A pause takes a Diff, which is the whole image of a guest that booted fresh: its holes are pages it never wrote (SHARD-450).
func TestAPauseOfABootedVMMWritesAWholeDiff(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec := h.runSnapshotted(t)
	dir := t.TempDir()

	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if got, want := snapshots(t, spec.StateDir), []string{"Diff onto 0"}; !slices.Equal(got, want) {
		t.Fatalf("the vmm took %q, want %q", got, want)
	}
	if got := memoryOf(t, dir); got != "Diff\n" {
		t.Fatalf("the snapshot memory = %q, want the one Diff", got)
	}
}

// Each pause after a resume merges its Diff into a copy of the memory the vmm loaded, so a chain keeps every earlier page (SHARD-450, SHARD-451).
func TestAChainOfPausesAndResumesKeepsWhatEachPauseWrote(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec := h.runSnapshotted(t)
	dir := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	for round := range 2 {
		if err := h.provider.Resume(t.Context(), spec.ID, dir); err != nil {
			t.Fatalf("Resume %d: %v", round, err)
		}
		if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
			t.Fatalf("Pause %d after a resume: %v", round, err)
		}
	}

	if got, want := snapshots(t, spec.StateDir), []string{"Diff onto 0", "Diff onto 1", "Diff onto 2"}; !slices.Equal(got, want) {
		t.Fatalf("the vmms took %q, want %q", got, want)
	}
	if got := memoryOf(t, dir); got != "Diff\nDiff\nDiff\n" {
		t.Fatalf("the snapshot memory = %q, want every Diff of the chain", got)
	}
}

// A fork of a resumed sandbox loads its snapshot, so the fork's own pause merges onto that memory too (SHARD-450).
func TestAPauseOfAForkMergesOntoTheSnapshotItLoaded(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec := h.runSnapshotted(t)
	dir := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause after the resume: %v", err)
	}
	fork := h.forkSpec(t)
	watchSnapshots(t, fork)
	if err := h.provider.Fork(t.Context(), dir, fork); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	forkDir := t.TempDir()

	if err := h.provider.Pause(t.Context(), fork.ID, forkDir); err != nil {
		t.Fatalf("Pause of the fork: %v", err)
	}
	if got, want := snapshots(t, fork.StateDir), []string{"Diff onto 2"}; !slices.Equal(got, want) {
		t.Fatalf("the fork took %q, want %q", got, want)
	}
	if got := memoryOf(t, forkDir); got != "Diff\nDiff\nDiff\n" {
		t.Fatalf("the fork's snapshot memory = %q, want the source's two and its own", got)
	}
	if got := memoryOf(t, dir); got != "Diff\nDiff\n" {
		t.Fatalf("the source's snapshot memory = %q after the fork paused, want it untouched", got)
	}
}

// The Diff takes the pages that are resident, so a pause refuses a vmm whose cgroup it cannot hold at no swap, by the cgroup's name, before it freezes the guest (SHARD-450).
func TestAPauseNeedsTheVMMsCgroupToSwapNothing(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec := h.runSnapshotted(t)
	root := t.TempDir()
	cgroup, err := firecracker.BoundVMM(root, spec.ID, spec.Resources)
	if err != nil {
		t.Fatal(err)
	}
	h.provider.SetCgroupRoot(root)
	t.Cleanup(func() { h.provider.SetCgroupRoot("") })
	swap := filepath.Join(cgroup, "memory.swap.max")
	if err := os.Remove(swap); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(swap, 0o700); err != nil {
		t.Fatal(err)
	}

	err = h.provider.Pause(t.Context(), spec.ID, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), cgroup) {
		t.Fatalf("Pause over a cgroup that cannot be held at no swap = %v, want a refusal that names %s", err, cgroup)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status after the refused pause = %+v, %v, want running", status, err)
	}

	if err := os.Remove(swap); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(swap, []byte("max"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Pause(t.Context(), spec.ID, t.TempDir()); err != nil {
		t.Fatalf("Pause over a cgroup that may swap = %v, want it pinned to none and the pause done", err)
	}
	if got, err := os.ReadFile(swap); err != nil || string(got) != "0" {
		t.Fatalf("memory.swap.max after the pause = %q, %v, want 0", got, err)
	}
	if got, want := snapshots(t, spec.StateDir), []string{"Diff onto 0"}; !slices.Equal(got, want) {
		t.Fatalf("the vmm took %q, want only the one pause that passed", got)
	}
}

// A pause writes 0 to memory.swap.max and reads it back; a fifo answers that read with "max", as a kernel that ignored the write would, and the pause is refused by the cgroup's name (SHARD-450).
func TestAPauseRefusesACgroupWhoseSwapDoesNotReadBackZero(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec := h.runSnapshotted(t)
	root := t.TempDir()
	cgroup, err := firecracker.BoundVMM(root, spec.ID, spec.Resources)
	if err != nil {
		t.Fatal(err)
	}
	h.provider.SetCgroupRoot(root)
	t.Cleanup(func() { h.provider.SetCgroupRoot("") })
	swap := filepath.Join(cgroup, "memory.swap.max")
	if err := os.Remove(swap); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(swap, 0o600); err != nil {
		t.Fatal(err)
	}
	answered := make(chan error, 1)
	go func() { answered <- answerSwap(swap, "max") }()

	err = h.provider.Pause(t.Context(), spec.ID, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), cgroup) || !strings.Contains(err.Error(), "reads -1") {
		t.Fatalf("Pause over a cgroup whose swap reads back max = %v, want a refusal that names %s", err, cgroup)
	}
	select {
	case err := <-answered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pause never read memory.swap.max back")
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status after the refused pause = %+v, %v, want running", status, err)
	}
}

// answerSwap takes the pause's write of memory.swap.max from the fifo, then answers its read with value.
func answerSwap(fifo, value string) error {
	if _, err := os.ReadFile(fifo); err != nil {
		return err
	}

	return os.WriteFile(fifo, []byte(value), 0o600)
}

// Only firecracker 1.13 and newer take a Diff without a dirty-page log, so a daemon refuses an older one by its version (SHARD-450).
func TestADaemonRefusesAFirecrackerOlderThan113(t *testing.T) {
	h := newHarness(t)
	t.Setenv(fakeVersionEnv, "1.12.1")

	_, err := firecracker.New(h.config())
	if err == nil || !strings.Contains(err.Error(), "1.12.1") || !strings.Contains(err.Error(), "1.13.0") {
		t.Fatalf("New over firecracker 1.12.1 = %v, want a refusal that names both versions", err)
	}
}

// runSnapshotted runs a long entrypoint with the fake vmm noting each snapshot it writes.
func (h *harness) runSnapshotted(t *testing.T) models.SandboxSpec {
	t.Helper()

	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	watchSnapshots(t, spec)
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	return spec
}

// watchSnapshots has the fake vmm of s note each snapshot from here on.
func watchSnapshots(t *testing.T, s models.SandboxSpec) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(s.StateDir, snapshotsFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func snapshots(t *testing.T, dir string) []string {
	t.Helper()

	read, err := os.ReadFile(filepath.Join(dir, snapshotsFile))
	if err != nil {
		t.Fatal(err)
	}

	return strings.Split(strings.TrimSpace(string(read)), "\n")
}

func memoryOf(t *testing.T, dir string) string {
	t.Helper()

	read, err := os.ReadFile(filepath.Join(dir, "memory"))
	if err != nil {
		t.Fatal(err)
	}

	return string(read)
}
