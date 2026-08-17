package state_test

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/state"
)

func newSandbox() models.Sandbox {
	return models.Sandbox{
		Image:         "docker.io/library/alpine:3.20",
		Provider:      "gvisor",
		State:         models.StateRunning,
		PID:           4242,
		NetnsPath:     "/var/run/netns/sb",
		Address:       netip.MustParsePrefix("10.88.0.7/24"),
		HostInterface: "veth-sb",
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

func create(t *testing.T, r *state.Repository) models.Sandbox {
	t.Helper()

	sb, err := r.Create(newSandbox())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	return sb
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

func TestNewCreatesTheLayout(t *testing.T) {
	_, root := repo(t)

	for _, dir := range []string{"sandboxes", "snapshots", "locks"} {
		info, err := os.Stat(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}

		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}

		if info.Mode().Perm() != 0o750 {
			t.Errorf("%s is %v, want 0750", dir, info.Mode().Perm())
		}
	}
}

func TestCreateGeneratesAHumanReadableID(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	if sb.ID == "" {
		t.Fatal("Create returned an empty id")
	}

	if strings.Count(sb.ID, "-") != 2 {
		t.Errorf("the id %q does not read as adjective-noun-suffix", sb.ID)
	}

	got, err := r.Get(sb.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", sb.ID, err)
	}

	if got.ID != sb.ID {
		t.Errorf("the stored id is %q, want %q", got.ID, sb.ID)
	}
}

func TestCreateRefusesAnIDTheCallerSet(t *testing.T) {
	r, _ := repo(t)

	sb := newSandbox()
	sb.ID = "chosen-by-hand"

	if _, err := r.Create(sb); err == nil {
		t.Fatal("Create took an id from the caller, and only the repository may generate one")
	}
}

func TestCreateRefusesAnUnknownState(t *testing.T) {
	r, _ := repo(t)

	sb := newSandbox()
	sb.State = "melting"

	if _, err := r.Create(sb); err == nil {
		t.Fatal("Create accepted an unknown state")
	}
}

func TestCreateAndGetRoundTrip(t *testing.T) {
	r, _ := repo(t)
	want := create(t, r)

	got, err := r.Get(want.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt is %v, want %v", got.CreatedAt, want.CreatedAt)
	}

	got.CreatedAt, want.CreatedAt = time.Time{}, time.Time{}
	if got != want {
		t.Errorf("the record came back as %+v, want %+v", got, want)
	}
}

func TestStateSurvivesAProcessRestart(t *testing.T) {
	r, root := repo(t)
	want := create(t, r)

	// A second repository over the same root is what the next shard process sees.
	restarted, err := state.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := restarted.Get(want.ID)
	if err != nil {
		t.Fatalf("Get after the restart: %v", err)
	}

	if !got.CreatedAt.Equal(want.CreatedAt) || got.ID != want.ID || got.Address != want.Address {
		t.Errorf("the record changed over the restart: %+v", got)
	}
}

func TestAFinishedEntrypointStaysRunning(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	err := r.Update(sb.ID, func(sb *models.Sandbox) error {
		sb.ExitStatus = &models.ExitStatus{Code: 137, Signal: syscall.SIGKILL}

		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := r.Get(sb.ID)
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
	sb := create(t, r)

	got, err := r.Get(sb.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ExitStatus != nil {
		t.Errorf("the exit status is %+v, want nil: the entrypoint never ran", *got.ExitStatus)
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	r, _ := repo(t)

	if _, err := r.Get("quiet-otter-0000"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Get of a missing sandbox: %v, want ErrNotFound", err)
	}
}

func TestEveryGeneratedIDIsUnique(t *testing.T) {
	r, _ := repo(t)

	const sandboxes = 64

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		ids  = map[string]bool{}
		errs = make(chan error, sandboxes)
	)

	for range sandboxes {
		wg.Go(func() {
			sb, err := r.Create(newSandbox())
			if err != nil {
				errs <- err

				return
			}

			mu.Lock()
			defer mu.Unlock()

			if ids[sb.ID] {
				errs <- errors.New("the id " + sb.ID + " came back twice")
			}

			ids[sb.ID] = true
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent Create: %v", err)
	}

	all, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(all) != sandboxes {
		t.Errorf("List returned %d records, want %d: a claim was lost", len(all), sandboxes)
	}
}

func TestListIsOrderedByIDAndIgnoresStrayEntries(t *testing.T) {
	r, root := repo(t)

	want := make([]string, 0, 3)
	for range 3 {
		want = append(want, create(t, r).ID)
	}

	slices.Sort(want)

	if err := os.WriteFile(filepath.Join(root, "sandboxes", "stray.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write the stray file: %v", err)
	}

	all, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	got := make([]string, 0, len(all))
	for _, sb := range all {
		got = append(got, sb.ID)
	}

	if !slices.Equal(got, want) {
		t.Errorf("List returned %v, want %v", got, want)
	}
}

func TestADirectoryWithNoRecordIsNotASandbox(t *testing.T) {
	r, root := repo(t)
	sb := create(t, r)

	for _, dir := range []string{"claimed-but-unwritten-0001", "not an id"} {
		if err := os.MkdirAll(filepath.Join(root, "sandboxes", dir), 0o750); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	all, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(all) != 1 || all[0].ID != sb.ID {
		t.Fatalf("List returned %+v, want %s alone", all, sb.ID)
	}

	err = r.Update(sb.ID, func(sb *models.Sandbox) error {
		sb.PID = 7

		return nil
	})
	if err != nil {
		t.Fatalf("Update beside a directory with no record: %v", err)
	}
}

func TestListOfAnEmptyRepositoryIsEmpty(t *testing.T) {
	r, _ := repo(t)

	all, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(all) != 0 {
		t.Errorf("List returned %+v, want nothing", all)
	}
}

func TestABadIDNeverLeavesTheRoot(t *testing.T) {
	r, root := repo(t)

	for _, id := range []string{"", "../escape", "with/slash", "with space", "..", strings.Repeat("a", 65)} {
		if _, err := r.Get(id); err == nil {
			t.Errorf("Get(%q) went through, and the id is not one plain directory name", id)
		}

		if _, err := r.Dir(id); err == nil {
			t.Errorf("Dir(%q) went through, and the caller hands that path to RemoveAll", id)
		}

		if _, err := r.SnapshotDir(id); err == nil {
			t.Errorf("SnapshotDir(%q) went through", id)
		}
	}

	entries, err := os.ReadDir(filepath.Dir(root))
	if err != nil {
		t.Fatalf("read the parent of the root: %v", err)
	}

	if len(entries) != 1 {
		t.Errorf("the parent of the root holds %d entries, want the root alone", len(entries))
	}
}

func TestUpdatePersists(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	err := r.Update(sb.ID, func(sb *models.Sandbox) error {
		sb.State = models.StatePaused
		sb.PID = 99

		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := r.Get(sb.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.State != models.StatePaused || got.PID != 99 {
		t.Errorf("the record is %+v, want paused with pid 99", got)
	}
}

func TestUpdateOfAMissingSandboxIsNotFound(t *testing.T) {
	r, _ := repo(t)

	err := r.Update("quiet-otter-0000", func(*models.Sandbox) error { return nil })
	if !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Update of a missing sandbox: %v, want ErrNotFound", err)
	}
}

func TestUpdateWritesNothingWhenMutateFails(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	stop := errors.New("stop")

	err := r.Update(sb.ID, func(sb *models.Sandbox) error {
		sb.PID = 1

		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("Update: %v, want the error mutate returned", err)
	}

	got, err := r.Get(sb.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.PID != sb.PID {
		t.Errorf("the pid is %d, want %d: a failed mutate wrote the record", got.PID, sb.PID)
	}
}

func TestUpdateRefusesToChangeTheID(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	err := r.Update(sb.ID, func(sb *models.Sandbox) error {
		sb.ID = "renamed-by-hand-0001"

		return nil
	})
	if err == nil {
		t.Fatal("Update changed the id, which would move the record out from under its own path")
	}
}

func TestUpdateRefusesAnUnknownState(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	err := r.Update(sb.ID, func(sb *models.Sandbox) error {
		sb.State = "melting"

		return nil
	})
	if err == nil {
		t.Fatal("Update accepted an unknown state")
	}
}

func TestDeleteRemovesTheRecordAndTheSnapshot(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	snapshot := snapshotDir(t, r, sb.ID)
	if err := os.MkdirAll(snapshot, 0o750); err != nil {
		t.Fatalf("create the snapshot directory: %v", err)
	}

	if err := r.Delete(sb.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	for _, dir := range []string{sandboxDir(t, r, sb.ID), snapshot} {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there after the delete", dir)
		}
	}

	if _, err := r.Get(sb.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Get after the delete: %v, want ErrNotFound", err)
	}
}

func TestDeleteOfAMissingSandboxIsNotFound(t *testing.T) {
	r, _ := repo(t)

	if err := r.Delete("quiet-otter-0000"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Delete of a missing sandbox: %v, want ErrNotFound", err)
	}
}

func TestConcurrentUpdatesLoseNothing(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	const writers = 32

	var wg sync.WaitGroup

	errs := make(chan error, writers*2)

	for range writers {
		wg.Go(func() {
			err := r.Update(sb.ID, func(sb *models.Sandbox) error {
				sb.PID++

				return nil
			})
			if err != nil {
				errs <- err
			}
		})

		wg.Go(func() {
			if _, err := r.Get(sb.ID); err != nil {
				errs <- err
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent access: %v", err)
	}

	got, err := r.Get(sb.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if want := sb.PID + writers; got.PID != want {
		t.Errorf("the pid is %d, want %d: an update was lost", got.PID, want)
	}
}

func TestTwoSandboxesDoNotWaitOnEachOther(t *testing.T) {
	r, _ := repo(t)
	first, second := create(t, r), create(t, r)

	held, release := make(chan struct{}), make(chan struct{})
	locked := make(chan error, 1)

	go func() {
		locked <- r.Update(first.ID, func(*models.Sandbox) error {
			close(held)
			<-release

			return nil
		})
	}()

	<-held

	defer func() {
		close(release)

		if err := <-locked; err != nil {
			t.Errorf("the update that held the lock: %v", err)
		}
	}()

	other := make(chan error, 1)

	go func() {
		other <- r.Update(second.ID, func(sb *models.Sandbox) error {
			sb.PID = 7

			return nil
		})
	}()

	select {
	case err := <-other:
		if err != nil {
			t.Fatalf("Update of the second sandbox: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second sandbox waited on the first, so the lock is not per sandbox")
	}
}

func TestDeleteRemovesTheLockFile(t *testing.T) {
	r, root := repo(t)
	sb := create(t, r)

	if err := r.Update(sb.ID, func(*models.Sandbox) error { return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	path := filepath.Join(root, "locks", sb.ID+".lock")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat the lock file after an update: %v", err)
	}

	if err := r.Delete(sb.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the lock file is still there after the delete: %v", err)
	}
}

func TestARecordIsReadableOnlyByItsOwner(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	info, err := os.Stat(filepath.Join(sandboxDir(t, r, sb.ID), "sandbox.json"))
	if err != nil {
		t.Fatalf("stat the record: %v", err)
	}

	if info.Mode().Perm() != 0o640 {
		t.Errorf("the record is %v, want 0640", info.Mode().Perm())
	}
}

func TestACorruptRecordIsAnError(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	path := filepath.Join(sandboxDir(t, r, sb.ID), "sandbox.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o640); err != nil {
		t.Fatalf("corrupt the record: %v", err)
	}

	if _, err := r.Get(sb.ID); err == nil {
		t.Fatal("Get returned a sandbox from a corrupt record")
	}
}
