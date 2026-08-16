package state_test

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/state"
)

func sandbox(id string) models.Sandbox {
	return models.Sandbox{
		ID:            id,
		Name:          id + "-name",
		Image:         "docker.io/library/alpine:3.20",
		Provider:      "gvisor",
		State:         models.StateRunning,
		PID:           4242,
		NetnsPath:     "/var/run/netns/" + id,
		Address:       netip.MustParsePrefix("10.88.0.7/24"),
		HostInterface: "veth-" + id,
		CreatedAt:     time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC),
	}
}

func repo(t *testing.T) (*state.Repository, string) {
	t.Helper()

	root := t.TempDir()

	r, err := state.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return r, root
}

func sandboxDir(t *testing.T, r *state.Repository, id string) string {
	t.Helper()

	dir, err := r.Dir(id)
	if err != nil {
		t.Fatalf("Dir(%s): %v", id, err)
	}

	return dir
}

func snapshotDir(t *testing.T, r *state.Repository, id string) string {
	t.Helper()

	dir, err := r.SnapshotDir(id)
	if err != nil {
		t.Fatalf("SnapshotDir(%s): %v", id, err)
	}

	return dir
}

func create(t *testing.T, r *state.Repository, sb models.Sandbox) {
	t.Helper()

	if err := r.Create(sb); err != nil {
		t.Fatalf("Create(%s): %v", sb.ID, err)
	}
}

func TestNewCreatesTheLayout(t *testing.T) {
	_, root := repo(t)

	for _, dir := range []string{"sandboxes", "snapshots"} {
		info, err := os.Stat(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}

		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}

		if perm := info.Mode().Perm(); perm != 0o750 {
			t.Errorf("%s has mode %o, want 750", dir, perm)
		}
	}
}

func TestCreateAndGetRoundTrip(t *testing.T) {
	r, _ := repo(t)
	want := sandbox("sb1")

	create(t, r, want)

	got, err := r.Get("sb1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt is %s, want %s", got.CreatedAt, want.CreatedAt)
	}

	got.CreatedAt, want.CreatedAt = time.Time{}, time.Time{}
	if got != want {
		t.Errorf("Get returned\n%+v\nwant\n%+v", got, want)
	}
}

func TestStateSurvivesAProcessRestart(t *testing.T) {
	r, root := repo(t)
	create(t, r, sandbox("sb1"))

	// A second repository over the same root is what the next shard process sees.
	restarted, err := state.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := restarted.Get("sb1")
	if err != nil {
		t.Fatalf("Get after the restart: %v", err)
	}

	want := sandbox("sb1")
	if !got.CreatedAt.Equal(want.CreatedAt) || got.ID != want.ID || got.Address != want.Address {
		t.Errorf("the record changed over the restart: %+v", got)
	}
}

func TestAFinishedEntrypointStaysRunning(t *testing.T) {
	r, _ := repo(t)
	create(t, r, sandbox("sb1"))

	err := r.Update("sb1", func(sb *models.Sandbox) error {
		sb.ExitStatus = &models.ExitStatus{Code: 137, Signal: syscall.SIGKILL}

		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := r.Get("sb1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.State != models.StateRunning {
		t.Errorf("state is %q, want running: the sandbox outlives its entrypoint", got.State)
	}

	if got.ExitStatus == nil {
		t.Fatal("the exit status is nil after it was recorded")
	}

	if got.ExitStatus.Code != 137 || got.ExitStatus.Signal != syscall.SIGKILL {
		t.Errorf("the exit status is %+v, want code 137 and signal SIGKILL", *got.ExitStatus)
	}
}

func TestAnExitStatusIsAbsentUntilOneHappens(t *testing.T) {
	r, _ := repo(t)
	create(t, r, sandbox("sb1"))

	got, err := r.Get("sb1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ExitStatus != nil {
		t.Errorf("the exit status is %+v, want nil: the entrypoint never ran", *got.ExitStatus)
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	r, _ := repo(t)

	if _, err := r.Get("sb1"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Get of a missing sandbox returned %v, want ErrNotFound", err)
	}
}

func TestCreateRefusesADuplicateID(t *testing.T) {
	r, _ := repo(t)
	create(t, r, sandbox("sb1"))

	second := sandbox("sb1")
	second.Name = "other"

	if err := r.Create(second); !errors.Is(err, state.ErrExists) {
		t.Fatalf("the second Create returned %v, want ErrExists", err)
	}
}

func TestCreateRefusesADuplicateName(t *testing.T) {
	r, _ := repo(t)
	create(t, r, sandbox("sb1"))

	second := sandbox("sb2")
	second.Name = sandbox("sb1").Name

	if err := r.Create(second); !errors.Is(err, state.ErrExists) {
		t.Fatalf("Create with a taken name returned %v, want ErrExists", err)
	}
}

func TestCreateAllowsManyUnnamedSandboxes(t *testing.T) {
	r, _ := repo(t)

	for _, id := range []string{"sb1", "sb2"} {
		sb := sandbox(id)
		sb.Name = ""

		create(t, r, sb)
	}

	all, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(all) != 2 {
		t.Fatalf("List returned %d sandboxes, want 2", len(all))
	}
}

func TestCreateRefusesABadID(t *testing.T) {
	r, root := repo(t)

	for _, id := range []string{"", "../escape", "sb/1", "sb 1", "sb.1"} {
		sb := sandbox("placeholder")
		sb.ID = id

		if err := r.Create(sb); err == nil {
			t.Errorf("Create with the id %q was accepted", id)
		}
	}

	entries, err := os.ReadDir(filepath.Dir(root))
	if err != nil {
		t.Fatalf("read the parent of the root: %v", err)
	}

	for _, entry := range entries {
		if entry.Name() == "escape" {
			t.Fatal("a record escaped the root")
		}
	}
}

func TestCreateRefusesAnUnknownState(t *testing.T) {
	r, _ := repo(t)

	sb := sandbox("sb1")
	sb.State = "exploded"

	if err := r.Create(sb); err == nil {
		t.Fatal("Create with an unknown state was accepted")
	}
}

func TestByName(t *testing.T) {
	r, _ := repo(t)
	create(t, r, sandbox("sb1"))
	create(t, r, sandbox("sb2"))

	got, err := r.ByName("sb2-name")
	if err != nil {
		t.Fatalf("ByName: %v", err)
	}

	if got.ID != "sb2" {
		t.Errorf("ByName returned %s, want sb2", got.ID)
	}

	if _, err := r.ByName("nobody"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("ByName of an unknown name returned %v, want ErrNotFound", err)
	}

	if _, err := r.ByName(""); err == nil {
		t.Error("ByName of the empty name was accepted")
	}
}

func TestListIsOrderedByIDAndIgnoresStrayEntries(t *testing.T) {
	r, root := repo(t)
	create(t, r, sandbox("sb2"))
	create(t, r, sandbox("sb1"))

	stray := filepath.Join(root, "sandboxes", "notes.txt")
	if err := os.WriteFile(stray, []byte("not a record"), 0o600); err != nil {
		t.Fatalf("write the stray file: %v", err)
	}

	all, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(all) != 2 || all[0].ID != "sb1" || all[1].ID != "sb2" {
		t.Fatalf("List returned %+v, want sb1 then sb2", all)
	}
}

// A half-done delete leaves a directory with no record. It must not take the other sandboxes down.
func TestADirectoryWithNoRecordIsNotASandbox(t *testing.T) {
	r, root := repo(t)
	create(t, r, sandbox("sb1"))

	for _, dir := range []string{"sb2", "not an id"} {
		if err := os.MkdirAll(filepath.Join(root, "sandboxes", dir), 0o750); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	all, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(all) != 1 || all[0].ID != "sb1" {
		t.Fatalf("List returned %+v, want sb1 alone", all)
	}

	err = r.Update("sb1", func(sb *models.Sandbox) error {
		sb.PID = 7

		return nil
	})
	if err != nil {
		t.Fatalf("Update beside a directory with no record: %v", err)
	}
}

func TestDirRefusesABadID(t *testing.T) {
	r, _ := repo(t)

	if _, err := r.Dir("../escape"); err == nil {
		t.Error("Dir built a path outside the root")
	}

	if _, err := r.SnapshotDir("../escape"); err == nil {
		t.Error("SnapshotDir built a path outside the root")
	}
}

func TestCreateRefusesABadName(t *testing.T) {
	r, _ := repo(t)

	sb := sandbox("sb1")
	sb.Name = "../escape"

	if err := r.Create(sb); err == nil {
		t.Fatal("Create with a name that is not a plain word was accepted")
	}
}

func TestListOfAnEmptyRepositoryIsEmpty(t *testing.T) {
	r, _ := repo(t)

	all, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(all) != 0 {
		t.Fatalf("List returned %d sandboxes, want none", len(all))
	}
}

func TestUpdatePersists(t *testing.T) {
	r, _ := repo(t)
	create(t, r, sandbox("sb1"))

	err := r.Update("sb1", func(sb *models.Sandbox) error {
		sb.State = models.StateStopped
		sb.PID = 0

		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := r.Get("sb1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.State != models.StateStopped || got.PID != 0 {
		t.Errorf("the record is %+v, want stopped and pid 0", got)
	}
}

func TestUpdateOfAMissingSandboxIsNotFound(t *testing.T) {
	r, _ := repo(t)

	err := r.Update("sb1", func(sb *models.Sandbox) error { return nil })
	if !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Update of a missing sandbox returned %v, want ErrNotFound", err)
	}
}

func TestUpdateWritesNothingWhenMutateFails(t *testing.T) {
	r, _ := repo(t)
	create(t, r, sandbox("sb1"))

	boom := errors.New("boom")

	err := r.Update("sb1", func(sb *models.Sandbox) error {
		sb.State = models.StateStopped

		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update returned %v, want boom", err)
	}

	got, err := r.Get("sb1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.State != models.StateRunning {
		t.Errorf("the record changed to %q after a failed mutate", got.State)
	}
}

func TestUpdateRefusesToRenameOntoATakenName(t *testing.T) {
	r, _ := repo(t)
	create(t, r, sandbox("sb1"))
	create(t, r, sandbox("sb2"))

	err := r.Update("sb2", func(sb *models.Sandbox) error {
		sb.Name = "sb1-name"

		return nil
	})
	if !errors.Is(err, state.ErrExists) {
		t.Fatalf("the rename returned %v, want ErrExists", err)
	}
}

func TestUpdateKeepsTheNameItAlreadyHolds(t *testing.T) {
	r, _ := repo(t)
	create(t, r, sandbox("sb1"))

	err := r.Update("sb1", func(sb *models.Sandbox) error {
		sb.State = models.StatePaused

		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestUpdateRefusesToChangeTheID(t *testing.T) {
	r, _ := repo(t)
	create(t, r, sandbox("sb1"))

	err := r.Update("sb1", func(sb *models.Sandbox) error {
		sb.ID = "sb2"

		return nil
	})
	if err == nil {
		t.Fatal("an update that changed the id was accepted")
	}
}

func TestDeleteRemovesTheRecordAndTheSnapshot(t *testing.T) {
	r, _ := repo(t)
	create(t, r, sandbox("sb1"))

	snapshot := snapshotDir(t, r, "sb1")
	if err := os.MkdirAll(snapshot, 0o750); err != nil {
		t.Fatalf("create the snapshot directory: %v", err)
	}

	if err := r.Delete("sb1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := r.Get("sb1"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Get after Delete returned %v, want ErrNotFound", err)
	}

	for _, dir := range []string{sandboxDir(t, r, "sb1"), snapshot} {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after Delete", dir)
		}
	}
}

func TestDeleteOfAMissingSandboxIsNotFound(t *testing.T) {
	r, _ := repo(t)

	if err := r.Delete("sb1"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Delete of a missing sandbox returned %v, want ErrNotFound", err)
	}
}

// The lock is what makes this pass: without it the readers and the writers lose each other's field.
func TestConcurrentUpdatesLoseNothing(t *testing.T) {
	r, _ := repo(t)
	create(t, r, sandbox("sb1"))

	const writers = 32

	var wg sync.WaitGroup

	errs := make(chan error, writers*2)

	for range writers {
		wg.Go(func() {
			err := r.Update("sb1", func(sb *models.Sandbox) error {
				sb.PID++

				return nil
			})
			if err != nil {
				errs <- err
			}
		})

		wg.Go(func() {
			if _, err := r.Get("sb1"); err != nil {
				errs <- err
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent access: %v", err)
	}

	got, err := r.Get("sb1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if want := sandbox("sb1").PID + writers; got.PID != want {
		t.Errorf("the pid is %d, want %d: an update was lost", got.PID, want)
	}
}

func TestARecordIsReadableOnlyByItsOwner(t *testing.T) {
	r, _ := repo(t)
	create(t, r, sandbox("sb1"))

	info, err := os.Stat(filepath.Join(sandboxDir(t, r, "sb1"), "sandbox.json"))
	if err != nil {
		t.Fatalf("stat the record: %v", err)
	}

	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("the record has mode %o, want 640", perm)
	}
}

func TestACorruptRecordIsAnError(t *testing.T) {
	r, _ := repo(t)
	create(t, r, sandbox("sb1"))

	if err := os.WriteFile(filepath.Join(sandboxDir(t, r, "sb1"), "sandbox.json"), []byte("{"), 0o640); err != nil {
		t.Fatalf("corrupt the record: %v", err)
	}

	if _, err := r.Get("sb1"); err == nil {
		t.Fatal("a corrupt record read as a sandbox")
	}

	if _, err := r.List(); err == nil {
		t.Fatal("List hid a corrupt record")
	}
}
