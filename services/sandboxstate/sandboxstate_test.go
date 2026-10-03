package sandboxstate_test

import (
	"errors"
	"fmt"
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

	for _, dir := range []string{"sandboxes", "snapshots"} {
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

// An admission runs on a claimed directory no verb lists, and its refusal leaves no directory and no name (SHARD-393).
func TestARefusedAdmissionLeavesNothing(t *testing.T) {
	r, _ := repo(t)
	sb := newSandbox()
	sb.Name = "builder"
	refused := errors.New("no room for the disk")

	var claimed string
	_, err := r.Create(sb, func(dir string) error {
		claimed = dir
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			t.Errorf("the admission ran before the directory %s was claimed: %v", dir, err)
		}
		if list, err := r.List(); err != nil || len(list) != 0 {
			t.Errorf("List during the admission = %v, %v, want nothing", list, err)
		}

		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("Create = %v, want the refusal", err)
	}
	if _, err := os.Stat(claimed); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory %s outlived the refusal: %v", claimed, err)
	}
	if _, err := r.Create(sb); err != nil {
		t.Fatalf("the refused create still holds the name builder: %v", err)
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

// A pause the daemon did not finish leaves <id>.tmp beside the snapshot, so a delete must take it too (SHARD-368).
func TestDeleteRemovesTheUnfinishedSnapshotTmp(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	tmp := snapshotDir(t, r, sb.ID) + ".tmp"
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		t.Fatalf("create the unfinished snapshot directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "checkpoint.img"), []byte("x"), 0o640); err != nil {
		t.Fatalf("plant the checkpoint: %v", err)
	}

	if err := r.Delete(sb.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s is still there after the delete", tmp)
	}
}

// SHARD-368: the start sweep removes an orphan .tmp no record reaches, and an invalid-id one, but keeps a
// .tmp a record still names, because that record's provider frees its own staging, not the sweep.
func TestSweepSnapshotTmpRemovesOrphansAndKeepsRecorded(t *testing.T) {
	r, root := repo(t)
	held := create(t, r)
	corrupt := create(t, r)

	snapshots := filepath.Join(root, "snapshots")
	orphan := filepath.Join(snapshots, "quiet-otter-0000.tmp")
	invalid := filepath.Join(snapshots, "NOT A VALID ID.tmp")
	heldTmp := snapshotDir(t, r, held.ID) + ".tmp"
	corruptTmp := snapshotDir(t, r, corrupt.ID) + ".tmp"
	for _, dir := range []string{orphan, invalid, heldTmp, corruptTmp} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("plant %s: %v", dir, err)
		}
	}

	// A record that will not decode still names its staging, so the sweep must keep that staging.
	if err := os.WriteFile(filepath.Join(sandboxDir(t, r, corrupt.ID), "sandbox.json"), []byte("{not json"), 0o640); err != nil {
		t.Fatalf("corrupt the record: %v", err)
	}

	var lines []string
	if err := r.SweepSnapshotTmp(func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatalf("SweepSnapshotTmp: %v", err)
	}

	for _, gone := range []string{orphan, invalid} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the orphan %s survived the sweep", gone)
		}
	}
	for _, name := range []string{heldTmp, corruptTmp} {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("the recorded %s did not survive the sweep: %v", name, err)
		}
	}
	if _, err := r.Get(held.ID); err != nil {
		t.Errorf("the sweep touched the record of %s: %v", held.ID, err)
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "will not read") }) {
		t.Errorf("the sweep reported %q, want a note that names the unreadable record", lines)
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "swept 2") && strings.Contains(l, "kept 2") }) {
		t.Errorf("the sweep reported %q, want swept 2 and kept 2", lines)
	}

	// A later start finds no orphan, but still names the staging it keeps, one line per daemon life.
	lines = nil
	if err := r.SweepSnapshotTmp(func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatalf("second SweepSnapshotTmp: %v", err)
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "swept 0") && strings.Contains(l, "kept 2") }) {
		t.Errorf("the second sweep reported %q, want swept 0 and kept 2", lines)
	}
}

// SHARD-381: a write that returns an error may still have landed the rename, so the generation must move or a reader keeps the old record.
func TestWriteMovesTheGenerationEvenWhenTheDurableWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test uses to force the write to fail")
	}

	r, _ := repo(t)
	sb := create(t, r)
	before := r.Generation()

	// A record directory that rejects a new temp file forces store.WriteFile to return an error, as a landed rename with a failed dir sync does.
	dir := sandboxDir(t, r, sb.ID)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := r.Update(sb.ID, func(sb *models.Sandbox) error {
		sb.PID = 4242

		return nil
	})
	if err == nil {
		t.Fatalf("Update over a write-protected directory: want an error, got nil")
	}
	if r.Generation() <= before {
		t.Errorf("the generation did not move after a failed write: before %d, now %d", before, r.Generation())
	}
}

// SHARD-381: a Create whose write fails after it may have landed the rename must bump the generation again once it removes the record, or a broker that rebuilt mid-cleanup keeps serving the failed sandbox and its secrets.
func TestCreateBumpsTheGenerationAfterItCleansUpAFailedWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test uses to force the write to fail")
	}

	r, _ := repo(t)
	before := r.Generation()

	// A umask that strips write makes claimID's new record directory reject the record file, so write fails as a landed rename with a failed dir sync does.
	old := syscall.Umask(0o222)
	defer syscall.Umask(old)

	if _, err := r.Create(newSandbox()); err == nil {
		t.Fatalf("Create over a umask that blocks the record write: want an error, got nil")
	}
	// write bumps once when the rename may have landed, and the cleanup must bump again once the record is gone.
	if moved := r.Generation() - before; moved < 2 {
		t.Errorf("the generation moved %d after a failed-write cleanup, want at least 2", moved)
	}
}

// SHARD-381: a delete that touches the disk must move the generation even on a later error, and a delete of an absent sandbox must not, so a reader rebuilds exactly when the set changed.
func TestDeleteMovesTheGenerationButANotFoundDeleteDoesNot(t *testing.T) {
	r, _ := repo(t)
	sb := create(t, r)

	before := r.Generation()
	if err := r.Delete(sb.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if r.Generation() <= before {
		t.Errorf("Delete did not move the generation: before %d, now %d", before, r.Generation())
	}

	steady := r.Generation()
	if err := r.Delete(sb.ID); !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Fatalf("second Delete: %v, want ErrNotFound", err)
	}
	if r.Generation() != steady {
		t.Errorf("a not-found delete moved the generation: was %d, now %d", steady, r.Generation())
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

func TestAWriteOfAMissingSandboxIsNotFound(t *testing.T) {
	r, _ := repo(t)

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

	// The daemon tells a partial list from a failed one by this type, so every row must carry it.
	var unreadable *sandboxstate.UnreadableError
	if !errors.As(err, &unreadable) || unreadable.ID != broken.ID {
		t.Errorf("the error is %T, want an UnreadableError for %s", err, broken.ID)
	}

	if len(all) != 1 || all[0].ID != good.ID {
		t.Fatalf("List returned %+v, want only the sandbox that reads", all)
	}
}

func TestARefusedReferenceIsAValidationError(t *testing.T) {
	r, _ := repo(t)

	var invalid *sandboxstate.ValidationError
	if _, err := r.Resolve("not valid!"); !errors.As(err, &invalid) {
		t.Errorf("Resolve of a bad reference got %T %v, want a ValidationError", err, err)
	}
	if _, err := r.Get(""); !errors.As(err, &invalid) {
		t.Errorf("Get of an empty id got %T %v, want a ValidationError", err, err)
	}
}

// SHARD-374: an upper-case ref or name folds onto another sandbox on a case-insensitive filesystem, so it is refused; an id shape is refused in any case.
func TestAMixedCaseNameOrReferenceIsRefused(t *testing.T) {
	r, _ := repo(t)

	var invalid *sandboxstate.ValidationError
	if _, err := r.Resolve("Morning-fern-b8b0"); !errors.As(err, &invalid) {
		t.Errorf("Resolve of an upper-case ref got %T %v, want a ValidationError", err, err)
	}
	if err := sandboxstate.ValidName("Morning-fern-b8b0"); !errors.As(err, &invalid) {
		t.Errorf("ValidName of a mixed-case id shape got %T %v, want a ValidationError", err, err)
	}
	if err := sandboxstate.ValidName("morning-fern-b8b0"); err == nil {
		t.Error("ValidName of a lower-case id shape got nil, want it refused")
	}
	if err := sandboxstate.ValidName("my-sandbox"); err != nil {
		t.Errorf("ValidName of a plain lower-case name got %v, want nil", err)
	}
}

// SHARD-46: a daemon asks the root what made its records before it picks a substrate for itself.
func TestRecordedProviderNamesWhatMadeTheRecords(t *testing.T) {
	r, root := repo(t)
	create(t, r)

	got, err := sandboxstate.RecordedProvider(root)
	if err != nil {
		t.Fatalf("RecordedProvider: %v", err)
	}
	if got != "gvisor" {
		t.Errorf("RecordedProvider = %q, want the provider of the record", got)
	}
}

func TestRecordedProviderOfARootWithoutRecords(t *testing.T) {
	_, root := repo(t)

	got, err := sandboxstate.RecordedProvider(root)
	if err != nil {
		t.Fatalf("RecordedProvider: %v", err)
	}
	if got != "" {
		t.Errorf("RecordedProvider = %q, want nothing from a root that holds no record", got)
	}
}

// A root a daemon has never prepared holds no tree at all, and asking must neither fail nor create one.
func TestRecordedProviderOfAFreshRootCreatesNothing(t *testing.T) {
	root := t.TempDir()

	got, err := sandboxstate.RecordedProvider(root)
	if err != nil {
		t.Fatalf("RecordedProvider: %v", err)
	}
	if got != "" {
		t.Errorf("RecordedProvider = %q, want nothing from a fresh root", got)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the root holds %d entries after the ask, want it untouched", len(entries))
	}
}

// SHARD-343: one record that will not decode must not stop a daemon reading what made the rest; a good record still names the substrate.
func TestRecordedProviderSkipsAnUndecodableRecord(t *testing.T) {
	r, root := repo(t)
	a, b := create(t, r), create(t, r)

	// Corrupt whichever id sorts first, so the undecodable record is read before the good one.
	ids := []string{a.ID, b.ID}
	slices.Sort(ids)
	brokenID := ids[0]

	path := filepath.Join(sandboxDir(t, r, brokenID), "sandbox.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o640); err != nil {
		t.Fatalf("corrupt the record: %v", err)
	}

	got, err := sandboxstate.RecordedProvider(root)
	if got != "gvisor" {
		t.Errorf("RecordedProvider = %q, want the provider the good record names", got)
	}

	var unreadable *sandboxstate.UnreadableError
	if !errors.As(err, &unreadable) || unreadable.ID != brokenID {
		t.Errorf("RecordedProvider error = %T %v, want an UnreadableError for %s", err, err, brokenID)
	}
}

// SHARD-343: a root where no record decodes selects as a root with no records, not a fatal read.
func TestRecordedProviderOfARootWhereNoRecordDecodes(t *testing.T) {
	r, root := repo(t)
	sb := create(t, r)

	path := filepath.Join(sandboxDir(t, r, sb.ID), "sandbox.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o640); err != nil {
		t.Fatalf("corrupt the record: %v", err)
	}

	got, err := sandboxstate.RecordedProvider(root)
	if got != "" {
		t.Errorf("RecordedProvider = %q, want nothing when no record decodes", got)
	}

	var unreadable *sandboxstate.UnreadableError
	if !errors.As(err, &unreadable) || unreadable.ID != sb.ID {
		t.Errorf("RecordedProvider error = %T %v, want an UnreadableError for %s", err, err, sb.ID)
	}
}

// SHARD-343: a corrupt record that sorts after a good one must still be reported, so the scan does not stop at the first provider.
func TestRecordedProviderReportsAnUndecodableRecordThatSortsLast(t *testing.T) {
	r, root := repo(t)
	a, b := create(t, r), create(t, r)

	// Corrupt whichever id sorts last, so a return at the first good record would miss it.
	ids := []string{a.ID, b.ID}
	slices.Sort(ids)
	brokenID := ids[1]

	path := filepath.Join(sandboxDir(t, r, brokenID), "sandbox.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o640); err != nil {
		t.Fatalf("corrupt the record: %v", err)
	}

	got, err := sandboxstate.RecordedProvider(root)
	if got != "gvisor" {
		t.Errorf("RecordedProvider = %q, want the provider the good record names", got)
	}

	var unreadable *sandboxstate.UnreadableError
	if !errors.As(err, &unreadable) || unreadable.ID != brokenID {
		t.Errorf("RecordedProvider error = %T %v, want an UnreadableError for %s", err, err, brokenID)
	}
}

// listResult is a Lister with a chosen result, so a ListReadable test can hand it any error shape.
type listResult struct {
	sandboxes []models.Sandbox
	err       error
}

func (l listResult) List() ([]models.Sandbox, error) { return l.sandboxes, l.err }

// SHARD-343: a join that carries a real error beside an unreadable record must fail closed, not pass partial state as success.
func TestListReadableFailsClosedOnAnErrorBesideAnUnreadableRecord(t *testing.T) {
	fatal := errors.New("read the sandboxes directory: permission denied")
	l := listResult{
		sandboxes: []models.Sandbox{{ID: "readable"}},
		err:       errors.Join(&sandboxstate.UnreadableError{ID: "broken", Err: errors.New("decode sandbox.json")}, fatal),
	}

	got, err := sandboxstate.ListReadable(l, nil)
	if !errors.Is(err, fatal) {
		t.Fatalf("ListReadable error = %v, want it to carry the fatal error", err)
	}
	if got != nil {
		t.Errorf("ListReadable returned %d records beside the fatal error, want none", len(got))
	}
}

// SHARD-343: a join of only unreadable records is skipped, and the readable records still come back.
func TestListReadableSkipsWhenEveryErrorIsAnUnreadableRecord(t *testing.T) {
	l := listResult{
		sandboxes: []models.Sandbox{{ID: "readable"}},
		err: errors.Join(
			&sandboxstate.UnreadableError{ID: "a", Err: errors.New("decode sandbox.json")},
			&sandboxstate.UnreadableError{ID: "b", Err: errors.New("decode sandbox.json")},
		),
	}

	got, err := sandboxstate.ListReadable(l, nil)
	if err != nil {
		t.Fatalf("ListReadable: %v", err)
	}
	if len(got) != 1 || got[0].ID != "readable" {
		t.Errorf("ListReadable = %+v, want the one readable record", got)
	}
}

// capture records each log line a dedup test produces, so an assertion can count them and read their text.
type capture struct {
	mu    sync.Mutex
	lines []string
}

func (c *capture) logf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

// SHARD-403: one unreadable record logs once across many lists, so a bad record does not flood the daemon log.
func TestListReadableLogsAnUnreadableRecordOncePerDaemonLife(t *testing.T) {
	rec := &capture{}
	ulog := sandboxstate.NewUnreadableLog(rec.logf)
	l := listResult{
		sandboxes: []models.Sandbox{{ID: "readable"}},
		err:       errors.Join(&sandboxstate.UnreadableError{ID: "broken", Err: errors.New("decode sandbox.json")}),
	}

	for range 3 {
		if _, err := sandboxstate.ListReadable(l, ulog); err != nil {
			t.Fatalf("ListReadable: %v", err)
		}
	}

	if len(rec.lines) != 1 {
		t.Fatalf("logged %d lines, want 1: %v", len(rec.lines), rec.lines)
	}
	if !strings.Contains(rec.lines[0], "broken") {
		t.Errorf("log line %q does not name the record", rec.lines[0])
	}
}

// SHARD-403: the same record logs again when its error text changes, so a new failure is not hidden.
func TestListReadableRelogsWhenTheRecordErrorChanges(t *testing.T) {
	rec := &capture{}
	ulog := sandboxstate.NewUnreadableLog(rec.logf)
	first := listResult{err: errors.Join(&sandboxstate.UnreadableError{ID: "broken", Err: errors.New("decode sandbox.json")})}
	second := listResult{err: errors.Join(&sandboxstate.UnreadableError{ID: "broken", Err: errors.New("permission denied")})}

	if _, err := sandboxstate.ListReadable(first, ulog); err != nil {
		t.Fatalf("ListReadable: %v", err)
	}
	if _, err := sandboxstate.ListReadable(second, ulog); err != nil {
		t.Fatalf("ListReadable: %v", err)
	}

	if len(rec.lines) != 2 {
		t.Fatalf("logged %d lines, want 2: %v", len(rec.lines), rec.lines)
	}
}

// SHARD-403: two bad records each log once, so a dedup does not swallow a second record.
func TestListReadableLogsEachDistinctRecordOnce(t *testing.T) {
	rec := &capture{}
	ulog := sandboxstate.NewUnreadableLog(rec.logf)
	l := listResult{
		err: errors.Join(
			&sandboxstate.UnreadableError{ID: "a", Err: errors.New("decode sandbox.json")},
			&sandboxstate.UnreadableError{ID: "b", Err: errors.New("decode sandbox.json")},
		),
	}

	for range 2 {
		if _, err := sandboxstate.ListReadable(l, ulog); err != nil {
			t.Fatalf("ListReadable: %v", err)
		}
	}

	if len(rec.lines) != 2 {
		t.Fatalf("logged %d lines, want 2: %v", len(rec.lines), rec.lines)
	}
}
