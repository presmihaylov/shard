package sandboxstate_test

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandboxstate"
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

func repo(t *testing.T) (*sandboxstate.Repository, string) {
	t.Helper()

	root := t.TempDir()

	r, err := sandboxstate.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return r, root
}

func create(t *testing.T, r *sandboxstate.Repository) models.Sandbox {
	t.Helper()

	sb, err := r.Create(newSandbox())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	return sb
}

func sandboxDir(t *testing.T, r *sandboxstate.Repository, id string) string {
	t.Helper()

	dir, err := r.Dir(id)
	if err != nil {
		t.Fatalf("Dir(%s): %v", id, err)
	}

	return dir
}

func snapshotDir(t *testing.T, r *sandboxstate.Repository, id string) string {
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

		// The umask of the host can only trim the mode, so assert that nothing wider got through.
		if perm := info.Mode().Perm(); perm&^0o750 != 0 {
			t.Errorf("%s is %v, which is wider than 0750", dir, perm)
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
	// DeepEqual, not ==: ExitStatus is a pointer, so == would compare two addresses.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the record came back as %+v, want %+v", got, want)
	}
}

func TestStateSurvivesAProcessRestart(t *testing.T) {
	r, root := repo(t)
	want := create(t, r)

	// A second repository over the same root is what the next shard process sees.
	restarted, err := sandboxstate.New(root)
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

// TestTheRecordReaderChild runs only as the child of TestStateSurvivesARealProcessRestart.
func TestTheRecordReaderChild(t *testing.T) {
	root, id := os.Getenv("SHARD_TEST_ROOT"), os.Getenv("SHARD_TEST_ID")
	if root == "" {
		t.Skip("this test is the child of TestStateSurvivesARealProcessRestart")
	}

	r, err := sandboxstate.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := r.Get(id)
	if err != nil {
		t.Fatalf("Get in the second process: %v", err)
	}

	want := newSandbox()
	want.ID = id

	if !reflect.DeepEqual(got, want) {
		t.Errorf("the second process read %+v, want %+v", got, want)
	}
}

// A second Repository is not a restart. This one re-execs the test binary and reads from there.
func TestStateSurvivesARealProcessRestart(t *testing.T) {
	r, root := repo(t)
	sb := create(t, r)

	cmd := exec.Command(os.Args[0], "-test.run", "^TestTheRecordReaderChild$", "-test.v")
	cmd.Env = append(os.Environ(), "SHARD_TEST_ROOT="+root, "SHARD_TEST_ID="+sb.ID)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the second process: %v\n%s", err, out)
	}

	// Without this the child could skip itself and the parent would still call that a pass.
	if !strings.Contains(string(out), "--- PASS: TestTheRecordReaderChild") {
		t.Errorf("the child did not run the read:\n%s", out)
	}
}

func TestAFinishedEntrypointStaysRunning(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	err := r.Update(sb.ID, func(sb *models.Sandbox) error {
		sb.ExitStatus = &models.ExitStatus{Code: 137, Signal: int(syscall.SIGKILL)}

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

	if got.ExitStatus.Code != 137 || got.ExitStatus.Signal != int(syscall.SIGKILL) {
		t.Errorf("the exit status is %+v, want code 137 and signal SIGKILL", *got.ExitStatus)
	}
}

// A clean exit is the case a value type with omitempty would drop, so it must round trip too.
func TestACleanExitIsRecordedAndNotMistakenForNone(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	err := r.Update(sb.ID, func(sb *models.Sandbox) error {
		sb.ExitStatus = &models.ExitStatus{}

		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := r.Get(sb.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ExitStatus == nil {
		t.Fatal("the exit status is nil, and the entrypoint exited 0: that is not the same as never running")
	}

	if got.ExitStatus.Code != 0 || got.ExitStatus.Signal != 0 {
		t.Errorf("the exit status is %+v, want code 0 and no signal", *got.ExitStatus)
	}

	if got.State != models.StateRunning {
		t.Errorf("state is %q, want running: the sandbox outlives its entrypoint", got.State)
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

	if _, err := r.Get("quiet-otter-0000"); !errors.Is(err, sandboxstate.ErrNotFound) {
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

		if err := r.Update(id, func(*models.Sandbox) error { return nil }); err == nil {
			t.Errorf("Update(%q) went through, and the id names the lock file too", id)
		}

		if err := r.Delete(id); err == nil {
			t.Errorf("Delete(%q) went through, and Delete hands that path to RemoveAll", id)
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
	if !errors.Is(err, sandboxstate.ErrNotFound) {
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

	if _, err := r.Get(sb.ID); !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Errorf("Get after the delete: %v, want ErrNotFound", err)
	}
}

func TestDeleteOfAMissingSandboxIsNotFound(t *testing.T) {
	r, _ := repo(t)

	if err := r.Delete("quiet-otter-0000"); !errors.Is(err, sandboxstate.ErrNotFound) {
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

func TestAWriteOfAMissingSandboxLeavesNoLockFile(t *testing.T) {
	r, root := repo(t)

	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"Update", func() error { return r.Update("quiet-heron-3f0a", func(*models.Sandbox) error { return nil }) }},
		{"Delete", func() error { return r.Delete("quiet-heron-3f0a") }},
	} {
		t.Run(call.name, func(t *testing.T) {
			if err := call.run(); !errors.Is(err, sandboxstate.ErrNotFound) {
				t.Fatalf("%s of a missing sandbox: %v", call.name, err)
			}

			path := filepath.Join(root, "locks", "quiet-heron-3f0a.lock")
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s left a lock file behind: %v", call.name, err)
			}
		})
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

// An operator must still see the sandboxes that read, because each one holds a process and a netns.
func TestOneCorruptRecordDoesNotHideTheOthers(t *testing.T) {
	r, _ := repo(t)
	broken, good := create(t, r), create(t, r)

	path := filepath.Join(sandboxDir(t, r, broken.ID), "sandbox.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o640); err != nil {
		t.Fatalf("corrupt the record: %v", err)
	}

	all, err := r.List()
	if err == nil {
		t.Error("List reported no error, and one record does not decode")
	}

	if !strings.Contains(err.Error(), broken.ID) {
		t.Errorf("the error does not name the corrupt sandbox: %v", err)
	}

	if len(all) != 1 || all[0].ID != good.ID {
		t.Fatalf("List returned %+v, want only the sandbox that reads", all)
	}
}

func TestHoldWaitsForTheHolder(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	release, err := r.Hold(t.Context(), sb.ID)
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, err := r.Hold(ctx, sb.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second Hold returned %v, want it to wait out its context", err)
	}

	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	again, err := r.Hold(t.Context(), sb.ID)
	if err != nil {
		t.Fatalf("Hold after the release: %v", err)
	}
	if err := again(); err != nil {
		t.Fatalf("release again: %v", err)
	}
}

// An Update inside a Hold must not wait on it, or every verb that writes the record would hang.
func TestAnUpdateInsideAHoldDoesNotWait(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	release, err := r.Hold(t.Context(), sb.ID)
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Errorf("release: %v", err)
		}
	}()

	done := make(chan error, 1)
	go func() { done <- r.Update(sb.ID, func(*models.Sandbox) error { return nil }) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the Update waited on the Hold")
	}
}

func TestHoldOfAMissingSandboxIsNotFoundAndLeavesNoLockFile(t *testing.T) {
	r, root := repo(t)

	if _, err := r.Hold(t.Context(), "quiet-heron-3f0a"); !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Fatalf("Hold of a missing sandbox: %v", err)
	}

	path := filepath.Join(root, "locks", "quiet-heron-3f0a.verb.lock")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Hold left a lock file behind: %v", err)
	}
}

func TestDeleteRemovesTheVerbLockFile(t *testing.T) {
	r, root := repo(t)
	sb := create(t, r)

	release, err := r.Hold(t.Context(), sb.ID)
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	path := filepath.Join(root, "locks", sb.ID+".verb.lock")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat the lock file under a hold: %v", err)
	}

	// rm deletes under its own hold, so the file goes while it is held.
	if err := r.Delete(sb.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the verb lock file is still there after the delete: %v", err)
	}
}

// An rm that waited on another rm wins a lock file it made itself, over a sandbox that is gone.
func TestHoldAfterADeleteIsNotFoundAndLeavesNoLockFile(t *testing.T) {
	r, root := repo(t)
	sb := create(t, r)

	release, err := r.Hold(t.Context(), sb.ID)
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	waited := make(chan error, 1)
	go func() {
		_, err := r.Hold(t.Context(), sb.ID)
		waited <- err
	}()

	// Give the second Hold time to pass its check and sit on the lock.
	time.Sleep(200 * time.Millisecond)

	if err := r.Delete(sb.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	select {
	case err := <-waited:
		if !errors.Is(err, sandboxstate.ErrNotFound) {
			t.Fatalf("the Hold that waited out the delete returned %v, want not found", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second Hold never returned")
	}

	path := filepath.Join(root, "locks", sb.ID+".verb.lock")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the Hold that lost left a lock file behind: %v", err)
	}
}
