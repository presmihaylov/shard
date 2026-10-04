package sandboxstate_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

func snapshots(t *testing.T) (*sandboxstate.Snapshots, string) {
	t.Helper()

	root := t.TempDir()

	s, err := sandboxstate.NewSnapshots(root)
	if err != nil {
		t.Fatalf("NewSnapshots: %v", err)
	}

	return s, root
}

func newSnapshot(name string) models.Snapshot {
	return models.Snapshot{
		Name:      name,
		Source:    "quiet-otter-1a2b",
		Image:     "docker.io/library/alpine:3.20",
		Digest:    "sha256:0123",
		Provider:  "gvisor",
		DiskMiB:   1024,
		CreatedAt: time.Date(2026, 10, 4, 6, 0, 0, 0, time.UTC),
	}
}

// fillWith writes one file of n bytes, the way a provider copies the layer it keeps.
func fillWith(n int) func(string) error {
	return func(files string) error {
		return os.WriteFile(filepath.Join(files, "upper"), make([]byte, n), 0o640)
	}
}

func TestSnapshotCreateKeepsTheFilesAndTheName(t *testing.T) {
	s, _ := snapshots(t)

	snap, err := s.Create(newSnapshot("base"), fillWith(8192))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if snap.ID == "" || snap.Size < 8192 {
		t.Errorf("Create returned %+v, want an id and a size of at least 8192", snap)
	}

	id, err := s.Resolve("base")
	if err != nil || id != snap.ID {
		t.Fatalf("Resolve(base) = %q, %v, want %s", id, err, snap.ID)
	}

	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != snap {
		t.Errorf("Get returned %+v, want %+v", got, snap)
	}

	files, err := s.Files(id)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if _, err := os.Stat(filepath.Join(files, "upper")); err != nil {
		t.Errorf("the filled file is gone: %v", err)
	}
}

func TestSnapshotCreateThatFailsToFillLeavesNothing(t *testing.T) {
	s, root := snapshots(t)

	_, err := s.Create(newSnapshot("base"), func(string) error { return errors.New("no space left on device") })
	if err == nil || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("Create failed with %v, want the fill error", err)
	}

	if entries, err := os.ReadDir(filepath.Join(root, "snapshots")); err != nil || len(entries) != 0 {
		t.Errorf("the failed create left %v (%v), want nothing", entries, err)
	}
	if _, err := s.Resolve("base"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := s.Get("base"); !errors.Is(err, sandboxstate.ErrSnapshotNotFound) {
		t.Errorf("the failed create left a record behind its name: %v", err)
	}
}

// A copy can take minutes, so a taken name is refused before the fill starts.
func TestSnapshotCreateRefusesATakenNameBeforeTheCopy(t *testing.T) {
	s, _ := snapshots(t)

	held, err := s.Create(newSnapshot("base"), fillWith(1))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	filled := false
	_, err = s.Create(newSnapshot("base"), func(string) error { filled = true; return nil })

	var taken *sandboxstate.NameTakenError
	if !errors.As(err, &taken) || !strings.Contains(err.Error(), "taken by snapshot "+held.ID) {
		t.Fatalf("the second create failed with %v, want the name taken by snapshot %s", err, held.ID)
	}
	if filled {
		t.Error("the second create copied before it refused the name")
	}
}

func TestSnapshotNameRefusesTheShapeOfAnID(t *testing.T) {
	err := sandboxstate.ValidSnapshotName("quiet-otter-1a2b")
	if err == nil || !strings.Contains(err.Error(), "snapshot name") {
		t.Errorf("an id-shaped name returned %v, want a refusal that names the snapshot name", err)
	}
}

// The two name spaces are separate: a sandbox and a snapshot may both be called web.
func TestSnapshotNamesAreApartFromSandboxNames(t *testing.T) {
	r, root := repo(t)

	sb := newSandbox()
	sb.Name = "web"
	if _, err := r.Create(sb); err != nil {
		t.Fatalf("Create the sandbox: %v", err)
	}

	s, err := sandboxstate.NewSnapshots(root)
	if err != nil {
		t.Fatalf("NewSnapshots: %v", err)
	}
	if _, err := s.Create(newSnapshot("web"), fillWith(1)); err != nil {
		t.Errorf("a snapshot named like a sandbox failed with %v", err)
	}
}

func TestSnapshotDeleteTakesTheNameAndTheFiles(t *testing.T) {
	s, root := snapshots(t)

	snap, err := s.Create(newSnapshot("base"), fillWith(1))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Delete(snap.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := s.Get(snap.ID); !errors.Is(err, sandboxstate.ErrSnapshotNotFound) {
		t.Errorf("Get after Delete returned %v, want ErrSnapshotNotFound", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "snapshot-names", "base")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the name outlived the snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "snapshots", snap.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the files outlived the snapshot: %v", err)
	}

	// The name is free again.
	if _, err := s.Create(newSnapshot("base"), fillWith(1)); err != nil {
		t.Errorf("a create over the freed name failed with %v", err)
	}
}

// A create a crash cut leaves a directory with no record: List skips it, and the start sweep removes it.
func TestSnapshotSweepRemovesWhatACutCreateLeft(t *testing.T) {
	s, root := snapshots(t)

	kept, err := s.Create(newSnapshot(""), fillWith(1))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cut := filepath.Join(root, "snapshots", "quiet-otter-0000")
	if err := os.MkdirAll(filepath.Join(cut, "files"), 0o750); err != nil {
		t.Fatalf("plant %s: %v", cut, err)
	}

	listed, err := s.List()
	if err != nil || len(listed) != 1 || listed[0].ID != kept.ID {
		t.Fatalf("List = %+v, %v, want only %s", listed, err, kept.ID)
	}

	var lines []string
	if err := s.Sweep(func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if _, err := os.Stat(cut); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the cut create survived the sweep: %v", err)
	}
	if _, err := s.Get(kept.ID); err != nil {
		t.Errorf("the sweep took the recorded snapshot: %v", err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "swept 1") {
		t.Errorf("the sweep reported %q, want swept 1", lines)
	}
}

// A hard link counts once, as the host holds it once.
func TestSnapshotSizeCountsAHardLinkOnce(t *testing.T) {
	s, _ := snapshots(t)

	snap, err := s.Create(newSnapshot(""), func(files string) error {
		path := filepath.Join(files, "upper")
		if err := os.WriteFile(path, make([]byte, 1<<20), 0o640); err != nil {
			return err
		}

		return os.Link(path, filepath.Join(files, "again"))
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if snap.Size < 1<<20 || snap.Size >= 2<<20 {
		t.Errorf("the size is %d, want one copy of 1 MiB", snap.Size)
	}
}
