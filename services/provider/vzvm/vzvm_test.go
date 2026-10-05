package vzvm_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/ext4"
	"github.com/presmihaylov/shard/pkg/pgroup"
	"github.com/presmihaylov/shard/pkg/pidpin/pidpintest"
	"github.com/presmihaylov/shard/pkg/reaper"
	"github.com/presmihaylov/shard/pkg/vz"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/conformance"
	"github.com/presmihaylov/shard/services/provider/vzvm"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/supervisor"
)

const stopGrace = 5 * time.Second

// harness is one provider over one short root: a unix socket path is 104 bytes at most, and t.TempDir is longer.
type harness struct {
	provider    *vzvm.Provider
	root        string
	disk        string
	shim        string
	saveRestore bool
	// log is what the provider logged, read while it still writes.
	log *safeBuffer

	next atomic.Int64
}

// safeBuffer is a log sink the test reads while the provider still writes it.
type safeBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
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
	reaper.Require(t)
	marks := filepath.Join(root, marksFile)
	if err := os.WriteFile(marks, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reaper.Note(harnessesFile, marks); err != nil {
		t.Fatal(err)
	}
	// A shim the test froze or killed reads no EOF of the run's pipe, and a killed one leaves its guest behind.
	t.Cleanup(func() {
		if err := reaper.End(func() (reaper.Marks, error) { return reaper.Read(marks) }); err != nil {
			t.Error(err)
		}
	})

	h := &harness{root: root, disk: baseDisk(t, root), shim: os.Args[0], saveRestore: saveRestore, log: &safeBuffer{}}
	h.open(t)

	return h
}

// open is a daemon start: a provider over the root, which holds nothing of an earlier one in memory.
func (h *harness) open(t *testing.T) *vzvm.Provider {
	t.Helper()

	p, err := vzvm.New(vzvm.Config{
		Shim:        h.shim,
		Kernel:      "kernel",
		Init:        initBinary,
		Dir:         h.root,
		Dirs:        h.stateDir,
		SaveRestore: h.saveRestore,
		Log:         log.New(h.log, "", 0),
	})
	if err != nil {
		t.Fatalf("open the provider: %v", err)
	}
	// The newest provider holds the live shims, so a spec's cleanup must stop through it.
	h.provider = p

	return p
}

// reopen is a daemon restart: the first provider lets go of its shims, and a second one adopts them.
func (h *harness) reopen(t *testing.T) models.Provider {
	t.Helper()

	if err := h.provider.Close(); err != nil {
		t.Fatalf("close the provider: %v", err)
	}

	return h.open(t)
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
		// Stop and Remove answer nil for a sandbox a subtest already removed, so an error here is a VM left running.
		ctx := context.Background()
		if err := h.provider.Stop(ctx, id, stopGrace); err != nil {
			t.Errorf("stop sandbox %s at cleanup: %v", id, err)
		}
		if err := h.provider.Remove(ctx, id); err != nil {
			t.Errorf("remove sandbox %s at cleanup: %v", id, err)
		}
	})

	return models.SandboxSpec{ID: id, StateDir: dir, RootDisk: h.disk, Entrypoint: entrypoint, Resources: models.Resources{MemoryMiB: 256, DiskMiB: 16}}
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
		EmptyDir: func(t *testing.T) string { return t.TempDir() },
		Shell:    func(script string) []string { return []string{"/bin/sh", "-c", script} },
		// The fake guest is a host process, so the suite writes under the root; a checkpoint here proves the verbs and not the disk.
		Scratch:       h.root,
		SharedScratch: true,
		Reopen:        h.reopen,
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

// A pruned image disk is refused by the sentinel the API answers with 404 and the pull hint, not a bare stat error.
func TestCreateOverAGoneImageDiskNamesTheImage(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "exit 0")
	if err := os.Remove(spec.RootDisk); err != nil {
		t.Fatal(err)
	}

	err := h.provider.Create(t.Context(), spec)
	if !errors.Is(err, models.ErrImageGone) || !strings.Contains(err.Error(), spec.ID) {
		t.Fatalf("Create = %v, want models.ErrImageGone and the sandbox named", err)
	}
}

// The bound needs room under the 32 MiB headroom, so a VM too small for one is refused by name.
func TestCreateRefusesAMemoryBoundBelowTheMinimum(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "exit 0")
	spec.Resources.MemoryMiB = 64

	err := h.provider.Create(t.Context(), spec)
	if err == nil || !strings.Contains(err.Error(), spec.ID) || !strings.Contains(err.Error(), "128 MiB") {
		t.Fatalf("Create = %v, want a refusal that names the sandbox and the minimum", err)
	}

	// Zero is unbounded on Linux; a VM has no unbounded memory, so the refusal names the provider and the field instead of a default.
	spec.Resources.MemoryMiB = 0
	err = h.provider.Create(t.Context(), spec)
	for _, want := range []string{spec.ID, "provider vz", "needs resources.memory_mib", "set it to 128 MiB or more"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Create with --memory 0 = %v, want %q named", err, want)
		}
	}

}

// The orchestrator asks before it writes a record, so a refused --memory leaves no failed sandbox in ls.
func TestCheckResourcesRefusesWhatCreateRefuses(t *testing.T) {
	h := newHarness(t)

	for _, res := range []models.Resources{{MemoryMiB: 0}, {MemoryMiB: 64}} {
		err := h.provider.CheckResources(res)
		if err == nil || !strings.Contains(err.Error(), "128") {
			t.Fatalf("CheckResources(%+v) = %v, want a refusal that names the minimum", res, err)
		}
	}
	if err := h.provider.CheckResources(models.Resources{MemoryMiB: 128}); err != nil {
		t.Fatalf("CheckResources(128) = %v, want nil", err)
	}
	err := h.provider.CheckResources(models.Resources{MemoryMiB: 128, DiskMiB: 130})
	if err == nil || !strings.Contains(err.Error(), "set resources.disk_mib to 128 MiB or 131 MiB") {
		t.Fatalf("CheckResources(--disk 130) = %v, want the nearest bounds", err)
	}
}

// An image with no PATH gets the OCI default, as the bundle gives it on Linux, so a named entrypoint resolves in the guest.
func TestCreateGivesAnImageWithoutAPathTheDefault(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "sh", "-c", "exit 0")
	spec.Env = []string{"HOME=/root"}

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(spec.StateDir, "vm.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Run struct {
			Env []string `json:"env"`
		} `json:"run"`
	}
	if err := json.Unmarshal(blob, &r); err != nil {
		t.Fatal(err)
	}
	want := []string{"HOME=/root", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	if !slices.Equal(r.Run.Env, want) {
		t.Fatalf("the record's env = %q, want %q", r.Run.Env, want)
	}
}

// The disk is copied apart from the memory, so a pause freezes the guest's root before it stops the VM, and every path that runs the guest again thaws it (SHARD-296).
func TestAPauseFreezesTheGuestAndEveryPathThatRunsItAgainThawsIt(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	thawed := func(dir, after string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(dir, frozenFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the guest's root is still frozen after %s: %v", after, err)
		}
	}

	snap := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, unfrozenFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the pause stopped a guest whose root still took writes: %v", err)
	}
	fork := h.newSpec(t)
	if err := h.provider.ForkCheckpoint(t.Context(), snap, fork); err != nil {
		t.Fatal(err)
	}
	forkDir, err := h.stateDir(fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	thawed(forkDir, "a fork")
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	thawed(dir, "a resume")

	// Without a disk to copy the checkpoint cannot complete, and the pause gives the guest back able to write.
	if err := os.Remove(filepath.Join(dir, "disk.img")); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err == nil || !strings.Contains(err.Error(), "copy the disk") {
		t.Fatalf("Pause without a disk = %v, want the copy failure", err)
	}
	thawed(dir, "a failed pause")
}

// A freeze that lands while its answer is lost fails the pause, and the guest's root is thawed over the stream the provider dials again.
func TestAFreezeWhoseAnswerIsLostIsThawed(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cutFreezeFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := h.provider.Pause(t.Context(), spec.ID, t.TempDir()); err == nil || !strings.Contains(err.Error(), "freeze the guest's root") {
		t.Fatalf("Pause over a lost freeze answer = %v, want the freeze failure", err)
	}
	deadline := time.Now().Add(stopGrace)
	for {
		_, err := os.Stat(filepath.Join(dir, frozenFile))
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the guest's root is still frozen %s after the pause failed: %v", stopGrace, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status after the failed pause = %+v, %v; want running", status, err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
}

// A reset while a pause that froze the root is in flight leaves the root frozen: the stream dialed again must not thaw it under the VM pause.
func TestAResetUnderAPauseLeavesTheRootFrozen(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, resetOnPauseFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := h.provider.Pause(t.Context(), spec.ID, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, unfrozenFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the reconnect thawed the root under the pause: %v", err)
	}
}

// A pause retried while a reconnect thaws a freeze whose answer was lost freezes after that thaw, so the thaw never undoes it.
func TestAPauseRetriedDuringARecoveryThawFreezesAfterIt(t *testing.T) {
	h := newHarness(t)
	reached, release := make(chan struct{}), make(chan struct{})
	h.provider.HoldRecovery(sync.OnceFunc(func() {
		close(reached)
		<-release
	}))
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	free := sync.OnceFunc(func() { close(release) })
	// A failure before the release would leave the reconnect held, and the stop of the sandbox behind it.
	t.Cleanup(free)
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{orderFile, holdDialsFile, cutFreezeFile} {
		if err := os.WriteFile(filepath.Join(dir, marker), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The held dials keep the reconnect out until the failed pause is done, so it is the reconnect that thaws.
	if err := h.provider.Pause(t.Context(), spec.ID, t.TempDir()); err == nil || !strings.Contains(err.Error(), "freeze the guest's root") {
		t.Fatalf("Pause over a lost freeze answer = %v, want the freeze failure", err)
	}
	if err := os.Remove(filepath.Join(dir, holdDialsFile)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-time.After(stopGrace):
		t.Fatalf("the reconnect did not choose to thaw the lost freeze within %s", stopGrace)
	}

	ctx, snap := t.Context(), t.TempDir()
	var retryErr error
	retried := make(chan struct{})
	go func() {
		defer close(retried)
		retryErr = h.provider.Pause(ctx, spec.ID, snap)
	}()
	awaitRetry(t, retried)
	free()
	<-retried
	if retryErr != nil {
		t.Fatalf("the retried pause: %v", retryErr)
	}
	order, err := os.ReadFile(filepath.Join(dir, orderFile))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Fields(string(order)), []string{supervisor.KindFreeze, supervisor.KindThaw, supervisor.KindFreeze}; !slices.Equal(got, want) {
		t.Fatalf("the guest read %q, want the lost freeze, the recovery's thaw, then the retry's freeze", got)
	}
}

// An exec while a pause holds the VM is refused by the pause's name, never dialed into a VM that cannot answer it (SHARD-478).
func TestAnExecInsideAPauseIsRefusedByName(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	hold := filepath.Join(dir, holdSaveFile)
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A failure inside the hold must still let the pause go, or the stop of the sandbox waits behind it.
	t.Cleanup(func() {
		if err := os.Remove(hold); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Error(err)
		}
	})

	paused := make(chan error, 1)
	go func() { paused <- h.provider.Pause(t.Context(), spec.ID, t.TempDir()) }()
	deadline := time.Now().Add(stopGrace)
	for {
		if _, err := os.Stat(filepath.Join(dir, savingFile)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pause did not reach its save within %s", stopGrace)
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	_, err = h.provider.Exec(ctx, spec.ID, models.ExecSpec{Argv: []string{"true"}})
	cancel()
	var notStarted *models.CommandNotStartedError
	if !errors.As(err, &notStarted) || notStarted.Code != models.CommandNotExecutableExitCode || !strings.Contains(notStarted.Reason, "a pause holds the sandbox frozen") {
		t.Errorf("Exec inside a pause = %v, want the pause's refusal with code %d", err, models.CommandNotExecutableExitCode)
	}
	requireSendsRefused(t, h.provider, spec.ID, "pause", "the pause")
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	if err := <-paused; err != nil {
		t.Fatalf("the pause after the refused exec: %v", err)
	}
}

// awaitRetry returns once the retried pause ended, or waits on a lock in the machine's freeze while the recovery holds it.
func awaitRetry(t *testing.T, retried <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(stopGrace)
	for !waitsInFreeze() {
		select {
		case <-retried:
			return
		case <-time.After(time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the retried pause neither ended nor waited in freeze within %s", stopGrace)
		}
	}
}

// waitsInFreeze says whether a goroutine is blocked on a mutex inside the machine's freeze.
func waitsInFreeze() bool {
	buf := make([]byte, 1<<20)
	for g := range strings.SplitSeq(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
		if strings.Contains(g, "[sync.Mutex.Lock") && strings.Contains(g, "vzvm.(*machine).freeze(") {
			return true
		}
	}

	return false
}

// A pause keeps the save, the disk and the identifier together; a stop of a paused sandbox leaves it stopped and the checkpoint whole.
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
	for _, name := range []string{"checkpoint.json", "vm.vzvmstate", "disk.img", "checkpoint.img"} {
		if _, err := os.Stat(filepath.Join(snap, name)); err != nil {
			t.Errorf("the checkpoint lacks %s: %v", name, err)
		}
	}
	if _, err := os.Stat(snap + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the pause left its staging directory: %v", err)
	}
	// The save ends the shim, so the substrate says stopped and the checkpoint marker is what says paused.
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after Pause = %+v, %v", status, err)
	}
	// A second Pause is a service retry, and finds nothing left to finish.
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatalf("a second Pause = %v, want a no-op", err)
	}
	if err := h.provider.Pause(t.Context(), spec.ID, t.TempDir()); err == nil || !strings.Contains(err.Error(), "no complete checkpoint") {
		t.Fatalf("a second Pause into an empty directory = %v, want a refusal", err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err == nil || !strings.Contains(err.Error(), "resume it first") {
		t.Fatalf("Start of a paused sandbox = %v, want a refusal", err)
	}

	fork := h.newSpec(t)
	if err := h.provider.ForkCheckpoint(t.Context(), snap, fork); err != nil {
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

// A restored guest is reseeded while its processes are still frozen, so no fork of one save draws from the key the save holds.
func TestAResumeReseedsTheGuestBeforeItThaws(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, orderFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	snap := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	order, err := os.ReadFile(filepath.Join(dir, orderFile))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Fields(string(order)), []string{supervisor.KindFreeze, supervisor.KindReseed, supervisor.KindThaw}; !slices.Equal(got, want) {
		t.Fatalf("the guest read %q, want the pause's freeze, then the reseed before the thaw", got)
	}
}

// A save made by an older guest restores with its processes unfrozen, and the restore still reseeds it.
func TestAResumeOfAnUnfrozenSaveStillReseeds(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}

	snap := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(snap, "vm.vzvmstate")
	held, err := os.ReadFile(saved)
	if err != nil {
		t.Fatal(err)
	}
	unfrozen, cut := strings.CutSuffix(string(held), "\n"+frozenFile)
	if !cut {
		t.Fatalf("the save holds %q, want a frozen guest to strip", held)
	}
	if err := os.WriteFile(saved, []byte(unfrozen), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, orderFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	order, err := os.ReadFile(filepath.Join(dir, orderFile))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Fields(string(order)), []string{supervisor.KindReseed}; !slices.Equal(got, want) {
		t.Fatalf("the guest read %q, want a reseed and no thaw", got)
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

// With no command the host sends an empty run: the guest is ready with nothing forked, an exec works, and the stop records no exit (SHARD-453).
func TestStartWithNoEntrypointRunsTheGuestAlone(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t)
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start with no entrypoint: %v", err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status after Start = %+v, %v, want running", status, err)
	}
	exit, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "exit 0"}})
	if err != nil || exit.Code != 0 {
		t.Fatalf("Exec with no entrypoint = %+v, %v", exit, err)
	}

	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
	if exit, err := h.provider.ExitStatus(t.Context(), spec.ID); err != nil || exit != nil {
		t.Fatalf("ExitStatus after the stop = %+v, %v, want none: nothing ran to exit", exit, err)
	}
	if exit, err := h.provider.Wait(t.Context(), spec.ID); !errors.Is(err, models.ErrNoExitStatus) {
		t.Fatalf("Wait after the stop = %+v, %v, want ErrNoExitStatus", exit, err)
	}
}

// A link close that fails after the VM is down is a log line, so the stop stands and one remove ends the sandbox (SHARD-389).
func TestALinkCloseFaultAfterTheVMIsDownNeverFailsTheStop(t *testing.T) {
	stop := func(h *harness, id string) error { return h.provider.Stop(t.Context(), id, stopGrace) }
	remove := func(h *harness, id string) error { return h.provider.Remove(t.Context(), id) }
	for name, end := range map[string]func(*harness, string) error{"stop": stop, "remove": remove} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
			if err := h.provider.Create(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			if err := h.provider.Start(t.Context(), spec.ID); err != nil {
				t.Fatal(err)
			}
			h.provider.FailLinkClose(spec.ID, errors.New("write vmnet-host: no buffer space available"))

			if err := end(h, spec.ID); err != nil {
				t.Fatalf("%s with a link close fault = %v, want nil", name, err)
			}
			status, err := h.provider.Status(t.Context(), spec.ID)
			want := models.Status{Exists: true, State: models.StateStopped}
			if name == "remove" {
				want = models.Status{}
			}
			if err != nil || status != want {
				t.Fatalf("Status after the %s = %+v, %v; want %+v", name, status, err, want)
			}
		})
	}
}

// A snapshot copies the disk a stop kept, and a create seeded from it boots that disk under a machine id of its own.
func TestASnapshotSeedsTheDiskOfANewSandbox(t *testing.T) {
	h := newHarness(t)
	source := h.newSpec(t, "/bin/sh", "-c", "exit 0")
	if err := h.provider.Create(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	files := t.TempDir()
	if err := h.provider.Snapshot(t.Context(), source.ID, files); err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Fatalf("Snapshot of a live source = %v, want a refusal", err)
	}
	if err := h.provider.Stop(t.Context(), source.ID, stopGrace); err != nil {
		t.Fatal(err)
	}

	// The fake guest never writes its disk, so the test writes what a guest would have.
	disk, err := os.OpenFile(filepath.Join(source.StateDir, "disk.img"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disk.WriteString("kept by the source"); err != nil {
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Snapshot(t.Context(), source.ID, files); err != nil {
		t.Fatalf("Snapshot of a stopped source: %v", err)
	}

	seeded := h.newSpec(t, "/bin/sh", "-c", "exit 0")
	seeded.Seed = files
	if err := h.provider.Create(t.Context(), seeded); err != nil {
		t.Fatalf("Create from the snapshot: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(source.StateDir, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(seeded.StateDir, "disk.img")); err != nil || !bytes.Equal(got, want) {
		t.Errorf("the seeded disk holds %d bytes (%v), want the %d the source kept", len(got), err, len(want))
	}
	if got, src := readVM(t, seeded.StateDir).MachineID, readVM(t, source.StateDir).MachineID; got == "" || got == src {
		t.Errorf("the seeded sandbox's machine id is %q, want one of its own (the source's is %q)", got, src)
	}
}

// vm is the part of the record a test compares, decoded from the file as the provider wrote it.
type vm struct {
	MachineID string             `json:"machine_id"`
	RootFS    string             `json:"rootfs"`
	Run       supervisor.RunSpec `json:"run"`
	Paused    bool               `json:"paused"`
}

func readVM(t *testing.T, dir string) vm {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(dir, "vm.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r vm
	if err := json.Unmarshal(blob, &r); err != nil {
		t.Fatal(err)
	}

	return r
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
		models.VerbFork:   h.provider.Fork(t.Context(), spec.ID, h.newSpec(t)),
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
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory whatever its mode says, so no exit is lost")
	}
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
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err == nil || !strings.Contains(err.Error(), "lost its lifecycle state") {
		t.Fatalf("Stop = %v, want the lost exit", err)
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

// guestGone has a daemon restart find the shim of a running sandbox answering and its guest out of reach, and returns the fresh provider and the shim's pid.
func guestGone(t *testing.T) (models.SandboxSpec, *vzvm.Provider, int) {
	t.Helper()
	h, spec, shim := runningShim(t)
	if err := h.provider.Close(); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(spec.StateDir, "guest", fmt.Sprintf("%d.sock", supervisor.ControlPort))
	if err := os.Rename(control, control+".off"); err != nil {
		t.Fatal(err)
	}
	// A failed test still lets the cleanup's stop reach the guest.
	t.Cleanup(func() {
		if err := os.Rename(control+".off", control); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("put the control socket back: %v", err)
		}
	})

	return spec, h.open(t), shim
}

// An adopt whose guest does not attach ends the shim and puts why on file, so a start boots the sandbox again (SHARD-577).
func TestAnAdoptWhoseGuestDoesNotAttachEndsTheShim(t *testing.T) {
	spec, p, shim := guestGone(t)

	status, err := p.Status(t.Context(), spec.ID)
	if err != nil || status.Alive() || !strings.Contains(status.SupervisorFailed, "its guest does not attach") {
		t.Fatalf("Status over a guest that does not attach = %+v, %v, want it stopped with the reason", status, err)
	}
	awaitExit(t, shim)
	if err := p.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start after the adopt ended the shim: %v", err)
	}
	status, err = p.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning || status.SupervisorFailed != "" {
		t.Fatalf("Status after the start = %+v, %v, want it running with no failure", status, err)
	}
}

// A rm over a restart that finds the guest out of reach ends the shim and removes the sandbox (SHARD-577).
func TestRemoveEndsAnAdoptedShimWhoseGuestDoesNotAttach(t *testing.T) {
	spec, p, shim := guestGone(t)

	if err := p.Remove(t.Context(), spec.ID); err != nil {
		t.Fatalf("Remove over a guest that does not attach: %v", err)
	}
	awaitExit(t, shim)
}

// A pause that cannot complete its checkpoint resumes the VM, keeps the last checkpoint and leaves no staging directory.
func TestAFailedPauseResumesTheSandboxAndKeepsTheLastCheckpoint(t *testing.T) {
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

	// Without a disk to copy the checkpoint cannot complete, and the pause must give the VM back.
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
		t.Fatalf("the last checkpoint changed under a failed pause: %v", err)
	}
	if err := h.provider.Pause(t.Context(), spec.ID, t.TempDir()); err == nil || !strings.Contains(err.Error(), "copy the disk") {
		t.Fatalf("a second Pause = %v, want the copy failure again, not an already-paused refusal", err)
	}
}

// A pause that crashed after its record leaves the staged checkpoint beside the old one and a shim over a suspended guest; the daemon comes back, the service retries, and that pause is finished.
func TestARetriedPauseAfterARestartFinishesTheOneACrashLeft(t *testing.T) {
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

	// The crash state by hand: a complete second checkpoint staged, a record that says paused, and the shim still up over a suspended guest.
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	staged := snap + ".tmp"
	if err := os.CopyFS(staged, os.DirFS(snap)); err != nil {
		t.Fatal(err)
	}
	setJSON(t, filepath.Join(staged, "checkpoint.json"), "pause", 2)
	setJSON(t, filepath.Join(dir, "vm.json"), "paused", true)
	setJSON(t, filepath.Join(dir, "vm.json"), "pauses", 2)
	shim, _, err := vz.Adopt(t.Context(), filepath.Join(dir, "shim.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shim.Pause(); err != nil {
		t.Fatal(err)
	}

	// The daemon comes back and the service retries the pause, which finishes the crashed one: the staged checkpoint goes in and the shim goes.
	again := h.open(t)
	if err := again.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	if _, err := shim.State(t.Context()); err == nil {
		t.Fatal("the shim the crashed pause left still answers")
	}
	if got := readJSON(t, filepath.Join(snap, "checkpoint.json"))["pause"]; got != 2.0 {
		t.Fatalf("the checkpoint in place is pause %v, want 2, the staged one", got)
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging directory is still there: %v", err)
	}
	status, err := again.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after the finishing Pause = %+v, %v; want stopped", status, err)
	}
	if err := again.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	status, err = again.Status(t.Context(), spec.ID)
	if err != nil || !status.Alive() {
		t.Fatalf("Status after Resume = %+v, %v; want alive", status, err)
	}

	// An older staged checkpoint, left by a swap whose cleanup failed, goes, and the one in place stays.
	if err := again.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(staged, os.DirFS(snap)); err != nil {
		t.Fatal(err)
	}
	setJSON(t, filepath.Join(staged, "checkpoint.json"), "pause", 1)
	if err := again.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	if got := readJSON(t, filepath.Join(snap, "checkpoint.json"))["pause"]; got != 3.0 {
		t.Fatalf("the checkpoint in place is pause %v, want 3", got)
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

// A fronted VM gets the same trust store the bundle plants on Linux, handed to the guest to write, and the variables that point at it.
func TestCreateHandsAFrontedGuestTheMergedTrustStore(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/true")
	spec.RootFS = t.TempDir()
	if err := os.MkdirAll(filepath.Join(spec.RootFS, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec.RootFS, "etc/ssl/certs/ca-certificates.crt"), []byte("image-roots\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec.ProxyCA = []byte("proxy-ca\n")

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(spec.StateDir, "vm.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Run struct {
			Env   []string `json:"env"`
			Trust struct {
				Path  string `json:"path"`
				Roots []byte `json:"roots"`
			} `json:"trust"`
		} `json:"run"`
	}
	if err := json.Unmarshal(blob, &r); err != nil {
		t.Fatal(err)
	}
	if r.Run.Trust.Path != "/etc/ssl/certs/ca-certificates.crt" || string(r.Run.Trust.Roots) != "image-roots\nproxy-ca\n" {
		t.Errorf("the record's trust = %q at %q, want the image roots then the proxy CA at the image path", r.Run.Trust.Roots, r.Run.Trust.Path)
	}
	for _, key := range bundle.TrustEnv {
		if !slices.Contains(r.Run.Env, key+"=/etc/ssl/certs/ca-certificates.crt") {
			t.Errorf("the record's env lacks %s: %q", key, r.Run.Env)
		}
	}
}

// A grant after the create edits the record the next start sends: the placeholder, the trust store, and the ungrant that takes the placeholder back.
func TestTheEnvironmentIsTheRecordTheNextStartSends(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/true")
	spec.RootFS = t.TempDir()
	if err := os.MkdirAll(filepath.Join(spec.RootFS, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec.RootFS, "etc/ssl/certs/ca-certificates.crt"), []byte("image-roots\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec.Env = []string{"HELD=1"}
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}

	env, err := h.provider.Environment(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.CanSetEnv("HELD"); err == nil {
		t.Error("CanSetEnv let a held name through")
	}
	if err := env.TrustProxy([]byte("proxy-ca\n")); err != nil {
		t.Fatal(err)
	}
	if err := env.SetEnv("TOKEN", "shard-placeholder"); err != nil {
		t.Fatal(err)
	}
	if err := env.SetEnv("TOKEN", "again"); err == nil {
		t.Error("SetEnv set a name the guest already holds")
	}

	read := func() (env []string, trust string) {
		t.Helper()
		blob, err := os.ReadFile(filepath.Join(spec.StateDir, "vm.json"))
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Run struct {
				Env   []string `json:"env"`
				Trust *struct {
					Roots []byte `json:"roots"`
				} `json:"trust"`
			} `json:"run"`
		}
		if err := json.Unmarshal(blob, &r); err != nil {
			t.Fatal(err)
		}
		if r.Run.Trust != nil {
			trust = string(r.Run.Trust.Roots)
		}

		return r.Run.Env, trust
	}
	got, trust := read()
	if !slices.Contains(got, "TOKEN=shard-placeholder") || !slices.Contains(got, "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt") || !slices.Contains(got, "HELD=1") {
		t.Errorf("the record's env = %q, want the placeholder, the trust variables and what the create set", got)
	}
	if trust != "image-roots\nproxy-ca\n" {
		t.Errorf("the record's trust = %q, want the image roots then the proxy CA", trust)
	}

	if err := env.RemoveEnv("TOKEN"); err != nil {
		t.Fatal(err)
	}
	got, trust = read()
	if slices.Contains(got, "TOKEN=shard-placeholder") || trust != "image-roots\nproxy-ca\n" {
		t.Errorf("after the ungrant env = %q, trust = %q; want the placeholder gone and the trust kept", got, trust)
	}

	if _, err := h.provider.Environment("sb-none"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("Environment of an unknown sandbox = %v, want does not exist", err)
	}
}

// A reset of the transport, as a sleep of the host can cause, ends every stream; the provider dials again and the sandbox goes on.
func TestADroppedStreamIsDialedAgainWhileTheVMRuns(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do echo tick; sleep 0.2; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	logged := awaitLog(t, h.provider, spec.ID, 0)

	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(status.PID, syscall.SIGUSR1); err != nil {
		t.Fatalf("reset the fake shim's streams: %v", err)
	}

	// The log must keep flowing on the stream the provider opened again, and the control stream must answer an exec.
	awaitLog(t, h.provider, spec.ID, logged)
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	exit, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "echo again"}, Stdout: out})
	written, _ := os.ReadFile(out.Name())
	if err != nil || exit.Code != 0 || !strings.Contains(string(written), "again") {
		t.Fatalf("Exec after the reset = %+v, %q, %v", exit, written, err)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status after the reset = %+v, %v; want running", status, err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
}

// A guest that floods its control stream past the bound is cut off with one log line, and the sandbox goes on (SHARD-390).
func TestAFloodedControlStreamIsLoggedOnceAndTheSandboxGoesOn(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do echo tick; sleep 0.2; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	logged := awaitLog(t, h.provider, spec.ID, 0)

	// The marker floods the stream the provider dials after the reset, past the state line that stream opens with.
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, floodFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(status.PID, syscall.SIGUSR1); err != nil {
		t.Fatalf("reset the fake shim's streams: %v", err)
	}

	line := "sandbox " + spec.ID + ": refused a control message from the guest"
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(h.log.String(), line) {
		if time.Now().After(deadline) {
			t.Fatalf("no refusal was logged within 10s; the log reads %q", h.log.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(h.log.String(), "1 MiB") {
		t.Errorf("the refusal does not name the bound: %q", h.log.String())
	}
	awaitLog(t, h.provider, spec.ID, logged)

	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	exit, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "echo again"}, Stdout: out})
	if err != nil || exit.Code != 0 {
		t.Fatalf("Exec after the refusal = %+v, %v", exit, err)
	}
	written, err := os.ReadFile(out.Name())
	if err != nil || !strings.Contains(string(written), "again") {
		t.Fatalf("the exec after the refusal wrote %q, %v", written, err)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status after the refusal = %+v, %v; want running", status, err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(h.log.String(), "refused"); n != 1 {
		t.Errorf("the provider logged %d refusals for one flood, want 1:\n%s", n, h.log.String())
	}
}

// A guest that floods every control stream, by oversized lines or by queued events, is dialed a few times a second at most, and exec and stop still answer (SHARD-408, SHARD-550).
func TestAGuestThatFloodsEveryControlStreamIsDialedAFewTimesASecondAtMost(t *testing.T) {
	for _, flooding := range []string{floodEveryFile, floodEventsFile} {
		t.Run(flooding, func(t *testing.T) {
			floodEveryControlStream(t, flooding)
		})
	}
}

func floodEveryControlStream(t *testing.T, flooding string) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do echo tick; sleep 0.2; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{dialsFile, flooding} {
		if err := os.WriteFile(filepath.Join(dir, marker), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(status.PID, syscall.SIGUSR1); err != nil {
		t.Fatalf("reset the fake shim's streams: %v", err)
	}

	const flood = 3 * time.Second
	time.Sleep(flood)
	dials, err := os.ReadFile(filepath.Join(dir, dialsFile))
	if err != nil {
		t.Fatal(err)
	}
	// With no wait between them the provider dials hundreds of times in the flood; the waits double from 100 ms, so it dials about 5.
	if n := strings.Count(string(dials), "\n"); n < 2 || n > 9 {
		t.Fatalf("the provider dialed the flooding guest %d times in %s, want 2 to 9", n, flood)
	}

	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	exit, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "echo again"}, Stdout: out})
	if err != nil || exit.Code != 0 {
		t.Fatalf("Exec during the flood = %+v, %v", exit, err)
	}
	written, err := os.ReadFile(out.Name())
	if err != nil || !strings.Contains(string(written), "again") {
		t.Fatalf("the exec during the flood wrote %q, %v", written, err)
	}
	// A refused stream takes no stop request, so the stop waits out its grace and forces the VM off.
	if err := h.provider.Stop(t.Context(), spec.ID, time.Second); err != nil {
		t.Fatalf("Stop during the flood: %v", err)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after the stop = %+v, %v; want stopped", status, err)
	}
}

// A reset that takes a while to settle answers each dial with a stream that ends at once; the provider keeps dialing, and an exit that landed meanwhile reaches Wait through the replayed state.
func TestAnExitDuringADroppedStreamReachesWait(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "echo up; sleep 0.5; exit 7")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	awaitLog(t, h.provider, spec.ID, 0)

	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(status.PID, syscall.SIGUSR2); err != nil {
		t.Fatalf("reset the fake shim's streams: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), stopGrace)
	defer cancel()
	exit, err := h.provider.Wait(ctx, spec.ID)
	if err != nil || exit.Code != 7 {
		t.Fatalf("Wait across the reset = %+v, %v; want code 7", exit, err)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status after the reset = %+v, %v; want running", status, err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
}

// An exit that lands while no daemon holds the shim reaches the next daemon through the state the guest opens with.
func TestAnExitWhileNoProviderHeldTheShimReachesWait(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "echo up; sleep 0.5; exit 7")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	awaitLog(t, h.provider, spec.ID, 0)
	if err := h.provider.Close(); err != nil {
		t.Fatal(err)
	}
	// Longer than the entrypoint, so the exit lands while no stream is open.
	time.Sleep(1500 * time.Millisecond)

	adopter := h.open(t)
	ctx, cancel := context.WithTimeout(t.Context(), stopGrace)
	defer cancel()
	exit, err := adopter.Wait(ctx, spec.ID)
	if err != nil || exit.Code != 7 {
		t.Fatalf("Wait through the adopting provider = %+v, %v; want code 7", exit, err)
	}
	if err := adopter.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
}

// A daemon that starts over a root whose shim is gone finds the sandbox stopped, which the reconcile then records.
func TestANewProviderFindsASandboxWhoseShimIsGoneStopped(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(status.PID, syscall.SIGTERM); err != nil {
		t.Fatalf("end the fake shim: %v", err)
	}
	awaitExit(t, status.PID)

	status, err = h.open(t).Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Exists || status.Alive() || status.PID != 0 {
		t.Fatalf("the new provider sees %+v, want the sandbox stopped with no pid", status)
	}
}

// A shim that takes the dial and never answers still ends on a stop, held or adopted after a restart: the provider kills it by the pid behind its socket (SHARD-349).
func TestStopEndsASandboxWhoseShimIsTooFrozenToAnswer(t *testing.T) {
	pidpintest.Require(t)
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			stopsAFrozenShim(t, restart)
		})
	}
}

func stopsAFrozenShim(t *testing.T, restart bool) {
	h, spec, shim := frozenShim(t, restart)

	began := time.Now()
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop with a frozen shim: %v", err)
	}
	if took := time.Since(began); took > stopGrace+10*time.Second {
		t.Errorf("Stop with a frozen shim took %s, want under the grace plus 10 s", took)
	}
	awaitExit(t, shim)
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.Alive() || status.State != models.StateStopped {
		t.Fatalf("Status after the stop = %+v, %v; want stopped", status, err)
	}
}

// After a daemon restart a shim too frozen to answer reads unresponsive with its pid within the startup bound and is never killed for it; a thaw lets the next lookup adopt it (SHARD-422).
func TestAnAdoptedShimTooFrozenToAnswerReadsUnresponsiveUntilItAnswers(t *testing.T) {
	h, spec, shim := frozenShim(t, true)
	pid := fmt.Sprintf("pid %d", shim)
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, acceptsFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// The bound the daemon's startup probe gives each sandbox.
	ctx, cancel := context.WithTimeout(t.Context(), sandbox.DefaultProbeBudget)
	defer cancel()
	began := time.Now()
	status, err := h.provider.Status(ctx, spec.ID)
	if err != nil || status.State != models.StateUnresponsive || status.PID != shim || !strings.Contains(status.Reason, pid) {
		t.Fatalf("Status over a frozen shim after a restart = %+v, %v; want unresponsive with the reason naming %s", status, err, pid)
	}
	if took := time.Since(began); took > 8*time.Second {
		t.Errorf("Status over a frozen shim after a restart took %s, want under 8 s", took)
	}
	if err := syscall.Kill(shim, 0); err != nil {
		t.Fatalf("the frozen shim %d is gone after a Status: %v, want it kept", shim, err)
	}
	if status, err := h.provider.Status(t.Context(), spec.ID); err != nil || status.State != models.StateUnresponsive || status.PID != shim {
		t.Fatalf("the next Status = %+v, %v; want unresponsive with the same shim", status, err)
	}
	// A start over a disk whose shim may thaw would boot a second VM on it.
	if err := h.provider.Start(t.Context(), spec.ID); err == nil || !strings.Contains(err.Error(), "unresponsive") || !strings.Contains(err.Error(), pid) {
		t.Errorf("Start over a frozen adopted shim = %v, want a refusal naming unresponsive and %s", err, pid)
	}

	if err := syscall.Kill(shim, syscall.SIGCONT); err != nil {
		t.Fatalf("thaw the fake shim: %v", err)
	}
	// The adopt's state request, the pid read and the one request every later lookup shared; a new dial per lookup would fill the queue.
	if queued := settledLines(t, filepath.Join(dir, acceptsFile)); queued != 3 {
		t.Errorf("the thawed shim accepted %d connections its socket queue held, want 3", queued)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning || status.PID != shim || status.Reason != "" {
		t.Fatalf("Status after the thaw = %+v, %v; want running, adopted with the same shim", status, err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop after the thaw: %v", err)
	}
	awaitExit(t, shim)
}

// A stop kills a frozen shim an adopt left unresponsive by its pid after one short probe, with no grace (SHARD-422).
func TestStopKillsAnAdoptedShimTooFrozenToAnswerAtOnce(t *testing.T) {
	pidpintest.Require(t)
	h, spec, shim := frozenShim(t, true)
	if status, err := h.provider.Status(t.Context(), spec.ID); err != nil || status.State != models.StateUnresponsive {
		t.Fatalf("Status over a frozen shim after a restart = %+v, %v; want unresponsive", status, err)
	}

	began := time.Now()
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop over an unresponsive adopted shim: %v", err)
	}
	if took := time.Since(began); took >= stopGrace {
		t.Errorf("Stop over an unresponsive adopted shim took %s, want it killed with no grace, under %s", took, stopGrace)
	}
	awaitExit(t, shim)
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.Alive() || status.State != models.StateStopped {
		t.Fatalf("Status after the stop = %+v, %v; want stopped", status, err)
	}
}

// Lookups that race once a silent adopted shim answers attach it once, so it gets one control stream and one log pump (SHARD-422).
func TestLookupsThatRaceAfterAThawAttachTheShimOnce(t *testing.T) {
	h, spec, shim := frozenShim(t, true)
	if status, err := h.provider.Status(t.Context(), spec.ID); err != nil || status.State != models.StateUnresponsive {
		t.Fatalf("Status over a frozen shim after a restart = %+v, %v; want unresponsive", status, err)
	}
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, controlsFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(shim, syscall.SIGCONT); err != nil {
		t.Fatalf("thaw the fake shim: %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range cap(errs) {
		wg.Go(func() {
			<-start
			status, err := h.provider.Status(t.Context(), spec.ID)
			if err == nil && (status.State != models.StateRunning || status.PID != shim) {
				err = fmt.Errorf("Status after the thaw = %+v, want running with pid %d", status, shim)
			}
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if controls := settledLines(t, filepath.Join(dir, controlsFile)); controls != 1 {
		t.Errorf("the thawed shim got %d control streams, want 1: each is an attach of its own", controls)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop after the thaw: %v", err)
	}
	awaitExit(t, shim)
}

// settledLines is the line count of path once it holds still for a second.
func settledLines(t *testing.T, path string) int {
	t.Helper()
	last, still := -1, time.Now()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		out, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Count(string(out), "\n")
		if lines != last {
			last, still = lines, time.Now()
		}
		if time.Since(still) >= time.Second {
			return lines
		}
	}
	t.Fatalf("%s still grows after 10 s, at %d lines", path, last)

	return 0
}

// A held shim too frozen to answer reads unresponsive with its pid and is never killed for it: verbs refuse it by name, a thaw makes it running again, and a stop kills it at once (SHARD-421).
func TestAHeldShimTooFrozenToAnswerReadsUnresponsiveUntilItAnswers(t *testing.T) {
	pidpintest.Require(t)
	h, spec, shim := frozenShim(t, false)
	pid := fmt.Sprintf("pid %d", shim)

	began := time.Now()
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateUnresponsive || status.PID != shim || !strings.Contains(status.Reason, pid) {
		t.Fatalf("Status over a frozen held shim = %+v, %v; want unresponsive with the reason naming %s", status, err, pid)
	}
	if took := time.Since(began); took > 8*time.Second {
		t.Errorf("Status over a frozen held shim took %s, want under 8 s", took)
	}
	if err := syscall.Kill(shim, 0); err != nil {
		t.Fatalf("the frozen shim %d is gone after a Status: %v, want it kept", shim, err)
	}

	began = time.Now()
	_, err = h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "unresponsive") || !strings.Contains(err.Error(), pid) {
		t.Errorf("Exec over a frozen held shim = %v, want a refusal naming unresponsive and %s", err, pid)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("Exec over a frozen held shim took %s, want under 10 s", took)
	}
	// The refusal carries the reason typed, so the daemon records it without a second probe bound (SHARD-424).
	began = time.Now()
	var silent *models.UnresponsiveError
	if err := h.provider.Pause(t.Context(), spec.ID, t.TempDir()); !errors.As(err, &silent) || !strings.Contains(silent.Reason, pid) {
		t.Errorf("Pause over a frozen held shim = %v, want the unresponsive refusal naming %s", err, pid)
	}
	if took := time.Since(began); took > 8*time.Second {
		t.Errorf("Pause over a frozen held shim took %s, want under 8 s", took)
	}

	if err := syscall.Kill(shim, syscall.SIGCONT); err != nil {
		t.Fatalf("thaw the fake shim: %v", err)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning || status.PID != shim || status.Reason != "" {
		t.Fatalf("Status after the thaw = %+v, %v; want running again with the same shim", status, err)
	}

	if err := syscall.Kill(shim, syscall.SIGSTOP); err != nil {
		t.Fatalf("freeze the fake shim again: %v", err)
	}
	if status, err := h.provider.Status(t.Context(), spec.ID); err != nil || status.State != models.StateUnresponsive {
		t.Fatalf("Status after the second freeze = %+v, %v; want unresponsive", status, err)
	}
	began = time.Now()
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop over an unresponsive shim: %v", err)
	}
	if took := time.Since(began); took >= stopGrace {
		t.Errorf("Stop over an unresponsive shim took %s, want it killed with no grace, under %s", took, stopGrace)
	}
	awaitExit(t, shim)
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.Alive() {
		t.Fatalf("Status after the stop = %+v, %v; want stopped", status, err)
	}
}

func TestStopEndsAShimFrozenLongerThanItsSocketQueueHolds(t *testing.T) {
	pidpintest.Require(t)
	h, spec, shim := frozenShim(t, false)
	// A frozen shim accepts nothing; once its socket queue holds 128, a macOS dial reads refused, as if no shim were there.
	for range 200 {
		h.provider.Probe(t.Context(), spec.ID, 10*time.Millisecond)
	}
	if status, err := h.provider.Status(t.Context(), spec.ID); err != nil || status.State != models.StateUnresponsive {
		t.Fatalf("Status after 200 probes of a frozen shim = %+v, %v; want unresponsive", status, err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop over a long frozen shim: %v", err)
	}
	awaitExit(t, shim)
}

// frozenShim starts a sandbox and freezes its shim, across a provider restart when asked, and answers the shim's pid.
func frozenShim(t *testing.T, restart bool) (*harness, models.SandboxSpec, int) {
	t.Helper()

	return frozenShimOn(t, newHarness(t), restart)
}

func frozenShimOn(t *testing.T, h *harness, restart bool) (*harness, models.SandboxSpec, int) {
	t.Helper()
	spec, shim := runningShimOn(t, h)
	// The daemon goes before the freeze, so the next one meets the frozen shim only by its socket.
	if restart {
		if err := h.provider.Close(); err != nil {
			t.Fatalf("close the provider: %v", err)
		}
	}
	if err := syscall.Kill(shim, syscall.SIGSTOP); err != nil {
		t.Fatalf("freeze the fake shim: %v", err)
	}
	if restart {
		h.open(t)
	}

	return h, spec, shim
}

// runningShim starts a sandbox whose fake guest and shim the test kills at its end, whatever a verb left, and answers the shim's pid.
func runningShim(t *testing.T) (*harness, models.SandboxSpec, int) {
	t.Helper()
	h := newHarness(t)
	spec, shim := runningShimOn(t, h)

	return h, spec, shim
}

func runningShimOn(t *testing.T, h *harness) (models.SandboxSpec, int) {
	t.Helper()
	spec := h.newSpec(t, "/bin/sh", "-c", `echo "pids $$ $PPID"; while true; do sleep 1; done`)
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	awaitLog(t, h.provider, spec.ID, 0)
	// The fake's guest is a host process the shim's kill leaves behind, which a VM's guest is not.
	path, err := h.provider.LogPath(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entrypoint, guest int
	if _, err := fmt.Sscanf(string(out), "pids %d %d", &entrypoint, &guest); err != nil {
		t.Fatalf("read the guest pids from %q: %v", out, err)
	}
	// A pid of 1 or less would signal every process this user owns, or this test's own group.
	if entrypoint <= 1 || guest <= 1 {
		t.Fatalf("the guest pids are %d and %d, want two real processes", entrypoint, guest)
	}
	t.Cleanup(func() {
		for _, group := range []int{entrypoint, guest} {
			if err := pgroup.Kill(group, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Errorf("end the fake guest's group %d: %v", group, err)
			}
		}
		if err := syscall.Kill(guest, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("end the fake guest %d: %v", guest, err)
		}
	})

	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A pid of 0 would signal this test's own group.
	shim := status.PID
	if shim <= 0 {
		t.Fatalf("Status = %+v, want the shim's pid", status)
	}
	t.Cleanup(func() {
		if err := pgroup.Kill(shim, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("end the fake shim %d: %v", shim, err)
		}
	})

	return spec, shim
}

// cuts are where a daemon killed inside a pause leaves the VM paused: before its record says so (SHARD-375), and after (SHARD-402).
var cuts = []struct {
	name     string
	recorded bool
}{{"before the record", false}, {"after the record", true}}

// A daemon killed inside a pause leaves a paused VM, under either record; stop and rm still end it.
func TestStopAndRemoveEndASandboxACutPauseLeft(t *testing.T) {
	for _, cut := range cuts {
		for _, verb := range []string{"stop", "rm", "stop a vm that refuses to resume"} {
			t.Run(cut.name+"/"+verb, func(t *testing.T) {
				h, spec, shim := cutPause(t, cut.recorded)
				dir, err := h.stateDir(spec.ID)
				if err != nil {
					t.Fatal(err)
				}
				if verb == "stop a vm that refuses to resume" {
					if err := os.WriteFile(filepath.Join(dir, refuseResumeFile), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				within(t, stopGrace+10*time.Second, verb+" after a cut pause", func() error {
					if verb == "rm" {
						return h.provider.Remove(t.Context(), spec.ID)
					}

					return h.provider.Stop(t.Context(), spec.ID, stopGrace)
				})
				awaitExit(t, shim)
			})
		}
	}
}

// The first probe after the restart resumes the VM, thaws the root and clears a paused record, since the pause never returned to the service.
func TestStatusAfterACutPauseRunsTheSandboxAgain(t *testing.T) {
	for _, cut := range cuts {
		t.Run(cut.name, func(t *testing.T) {
			h, spec, _ := cutPause(t, cut.recorded)

			var status models.Status
			within(t, 5*time.Second, "Status after a cut pause", func() error {
				var err error
				status, err = h.provider.Status(t.Context(), spec.ID)

				return err
			})
			if status.State != models.StateRunning {
				t.Fatalf("Status after a cut pause = %+v, want running", status)
			}
			dir, err := h.stateDir(spec.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, info, err := vz.Adopt(t.Context(), filepath.Join(dir, "shim.sock"))
			if err != nil || info.State != vz.StateRunning {
				t.Fatalf("the shim says %+v, %v; want its VM running", info, err)
			}
			if _, err := os.Stat(filepath.Join(dir, frozenFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the guest's root is still frozen after the probe: %v", err)
			}
			if paused, _ := readJSON(t, filepath.Join(dir, "vm.json"))["paused"].(bool); paused {
				t.Fatal("the record still says paused over a running VM")
			}

			// The sandbox takes a whole pause and resume again.
			snap := t.TempDir()
			if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
				t.Fatalf("Pause after the probe: %v", err)
			}
			if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
				t.Fatalf("Resume after the probe: %v", err)
			}
			status, err = h.provider.Status(t.Context(), spec.ID)
			if err != nil || status.State != models.StateRunning {
				t.Fatalf("Status after Pause and Resume = %+v, %v; want running", status, err)
			}
		})
	}
}

// cutPause leaves what a daemon killed inside a pause leaves: the root frozen, the VM paused, the record paused when recorded, and a new provider.
func cutPause(t *testing.T, recorded bool) (*harness, models.SandboxSpec, int) {
	t.Helper()
	h, spec, shim := runningShim(t)
	if err := h.provider.Close(); err != nil {
		t.Fatalf("close the provider: %v", err)
	}
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	client, _, err := vz.Adopt(t.Context(), filepath.Join(dir, "shim.sock"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := client.Connect(t.Context(), supervisor.ControlPort)
	if err != nil {
		t.Fatal(err)
	}
	control := supervisor.ControlOver(conn)
	if _, err := control.Next(); err != nil {
		t.Fatalf("read the guest's state: %v", err)
	}
	if err := errors.Join(control.Freeze(t.Context(), models.VerbPause), control.Close()); err != nil {
		t.Fatalf("freeze the guest's root: %v", err)
	}
	if _, err := client.Pause(); err != nil {
		t.Fatal(err)
	}
	if recorded {
		setJSON(t, filepath.Join(dir, "vm.json"), "paused", true)
		setJSON(t, filepath.Join(dir, "vm.json"), "pauses", 1)
	}
	h.open(t)

	return h, spec, shim
}

// within fails the test when verb errs or has not returned by d; the test's cleanup kills the guest a stuck verb waits on.
func within(t *testing.T, d time.Duration, what string, verb func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- verb() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(d):
		t.Fatalf("%s has not returned in %s", what, d)
	}
}

// Daemon restarts and a dropped stream under an entrypoint that never stops writing lose no line of the log and repeat none.
func TestTheLogKeepsEveryLineAcrossDaemonRestarts(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "i=0; while true; do echo $i; i=$((i+1)); done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	logged := awaitLog(t, h.provider, spec.ID, 0)
	for range 5 {
		if err := h.provider.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := h.open(t).Status(t.Context(), spec.ID); err != nil {
			t.Fatal(err)
		}
		logged = awaitLog(t, h.provider, spec.ID, logged)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(status.PID, syscall.SIGUSR1); err != nil {
		t.Fatalf("reset the fake shim's streams: %v", err)
	}
	logged = awaitLog(t, h.provider, spec.ID, logged)
	awaitLog(t, h.provider, spec.ID, logged)

	path, err := h.provider.LogPath(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The last line may still be on its way.
	lines := strings.Split(string(out), "\n")
	lines = lines[:len(lines)-1]
	for i, line := range lines {
		if line != strconv.Itoa(i) {
			t.Fatalf("line %d of %d is %q, want %d: the log lost or repeated output across a restart", i, len(lines), line, i)
		}
	}
}

// awaitLog blocks until the sandbox log holds more than seen bytes, and answers how many it holds.
func awaitLog(t *testing.T, p *vzvm.Provider, id string, seen int) int {
	t.Helper()

	path, err := p.LogPath(id)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(stopGrace)
	for time.Now().Before(deadline) {
		out, _ := os.ReadFile(path)
		if len(out) > seen {
			return len(out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the log of %s did not grow past %d bytes", id, seen)

	return seen
}

// awaitExit blocks until the process is gone, which for the fake shim is its socket gone too.
func awaitExit(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(stopGrace)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d did not exit", pid)
}

// A frozen shim whose socket queue a verb's dials filled refuses every dial after, which is not a shim gone (SHARD-423).
func TestRemoveEndsAFrozenShimWhoseSocketQueueIsFull(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only darwin refuses a dial into a full queue; linux makes it wait")
	}
	pidpintest.Require(t)
	cases := []struct {
		name              string
		restart, recorded bool
	}{
		{"the same daemon", false, true},
		{"a restarted daemon", true, true},
		{"an upgrade from a daemon that recorded no shim", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			installShim(t, h)
			h, spec, shim := frozenShimOn(t, h, c.restart)
			dir, err := h.stateDir(spec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !c.recorded {
				forgetShim(t, dir)
				upgrade(t, h.shim)
			}
			fillQueue(t, filepath.Join(dir, "shim.sock"))

			status, err := h.provider.Status(t.Context(), spec.ID)
			if err != nil || status.State != models.StateUnresponsive || status.PID != shim {
				t.Errorf("Status over a frozen shim with a full queue = %+v, %v; want unresponsive with pid %d", status, err, shim)
			}
			if err := h.provider.Remove(t.Context(), spec.ID); err != nil {
				t.Fatalf("Remove over a frozen shim with a full queue: %v", err)
			}
			awaitExit(t, shim)
		})
	}
}

// A retried pause ends the shim a crashed pause left by its recorded pid, though its full socket queue refuses every dial (SHARD-423).
func TestARetriedPauseEndsALeftoverShimWhoseSocketQueueIsFull(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only darwin refuses a dial into a full queue; linux makes it wait")
	}
	pidpintest.Require(t)
	h, spec, shim := frozenShim(t, true)
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The crash state by hand: a complete checkpoint, a record that says paused, and the shim still up.
	snap := t.TempDir()
	if err := os.WriteFile(filepath.Join(snap, "checkpoint.json"), []byte(`{"pause":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	setJSON(t, filepath.Join(dir, "vm.json"), "paused", true)
	fillQueue(t, filepath.Join(dir, "shim.sock"))

	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatalf("the retried Pause over a frozen leftover with a full queue: %v", err)
	}
	awaitExit(t, shim)
}

// A resume ends the shim a crashed pause of an older daemon left before it boots the save, though no pid is recorded and the queue is full (SHARD-423).
func TestAResumeEndsAnUnrecordedLeftoverShimWhoseSocketQueueIsFull(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only darwin refuses a dial into a full queue; linux makes it wait")
	}
	h, spec, shim := frozenShim(t, true)
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	forgetShim(t, dir)
	blob, err := os.ReadFile(filepath.Join(dir, "vm.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		MachineID string `json:"machine_id"`
	}
	if err := json.Unmarshal(blob, &r); err != nil {
		t.Fatal(err)
	}
	// The crash state by hand: a complete save of this machine, a record that says paused, and the shim still up.
	snap := t.TempDir()
	files := map[string]string{"checkpoint.json": `{"pause":1,"machine_id":"` + r.MachineID + `"}`, "vm.vzvmstate": r.MachineID, "checkpoint.img": ""}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(snap, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(dir, "disk.img"), filepath.Join(snap, "disk.img")); err != nil {
		t.Fatal(err)
	}
	setJSON(t, filepath.Join(dir, "vm.json"), "paused", true)
	fillQueue(t, filepath.Join(dir, "shim.sock"))

	if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatalf("Resume over an unrecorded leftover with a full queue: %v", err)
	}
	awaitExit(t, shim)
}

// A killed shim an older daemon booted leaves a socket that refuses every dial and no live process started on it, so it reads stopped (SHARD-423).
func TestAKilledShimWithNoRecordReadsStopped(t *testing.T) {
	h, spec, shim := frozenShim(t, true)
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	forgetShim(t, dir)
	if err := syscall.Kill(shim, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	awaitExit(t, shim)

	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped || status.PID != 0 {
		t.Errorf("Status over a killed shim with no record = %+v, %v; want stopped with no pid", status, err)
	}
}

// A live process with a shim's arguments for the socket of a killed shim, run from another file, is no shim: it reads stopped and Remove leaves it be (SHARD-423).
func TestAProcessThatOnlyClaimsTheSocketIsNoShim(t *testing.T) {
	h, spec, shim := frozenShim(t, true)
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	forgetShim(t, dir)
	if err := syscall.Kill(shim, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	awaitExit(t, shim)
	exited := impostor(t, filepath.Join(dir, "shim.sock"))

	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped || status.PID != 0 {
		t.Errorf("Status with an impostor on the socket = %+v, %v; want stopped with no pid", status, err)
	}
	if err := h.provider.Remove(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		t.Errorf("Remove ended the impostor: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// impostor runs a copy of this test binary under another name with the arguments Start gives the shim of socket.
func impostor(t *testing.T, socket string) <-chan error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "impostor")
	copyBinary(t, path)
	config, err := json.Marshal(vz.Config{Socket: socket})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "-config", string(config))
	cmd.Env = append(os.Environ(), fakeShimEnv+"="+impostorRole)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("end the impostor: %v", err)
		}
	})

	return exited
}

// installShim runs the shims from a copy of this test binary where a daemon installs its own, so an upgrade can replace it.
func installShim(t *testing.T, h *harness) {
	t.Helper()
	h.shim = filepath.Join(h.root, "shard-vz-shim")
	copyBinary(t, h.shim)
	h.reopen(t)
}

// upgrade renames a new shim over the file a live shim runs from, as vzshim.Install does.
func upgrade(t *testing.T, shim string) {
	t.Helper()
	copyBinary(t, shim+".new")
	if err := os.Rename(shim+".new", shim); err != nil {
		t.Fatal(err)
	}
}

func copyBinary(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o700); err != nil {
		t.Fatal(err)
	}
}

// forgetShim drops shim.json, which a daemon from before SHARD-423 never wrote.
func forgetShim(t *testing.T, dir string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, "shim.json")); err != nil {
		t.Fatal(err)
	}
}

// The cleanup of a failed boot kills a shim whose full socket queue refuses the stop, and does not read it gone (SHARD-423).
func TestAFailedBootKillsAShimWhoseSocketQueueIsFull(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only darwin refuses a dial into a full queue; linux makes it wait")
	}
	pidpintest.Require(t)
	dir, err := os.MkdirTemp("", "vzq") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "shim.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	// A process that never accepts on the socket stands for the frozen shim.
	stand := exec.Command("sleep", "60")
	stand.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := stand.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- stand.Wait() }()
	t.Cleanup(func() {
		if err := stand.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("end the stand-in shim: %v", err)
		}
	})
	fillQueue(t, socket)

	if err := vzvm.EndShim("a", vz.Open(socket), stand.Process.Pid); err != nil {
		t.Fatalf("EndShim over a shim with a full queue: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(stopGrace):
		t.Fatal("the shim outlived the cleanup of its failed boot")
	}
}

// The cleanup of a failed boot waits out a shim that dropped its socket and still runs, as one closing its last connections does (SHARD-530).
func TestAFailedBootWaitsForAShimPastItsSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "vzq") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	stand := exec.Command("sleep", "1")
	stand.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := stand.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- stand.Wait() }()
	t.Cleanup(func() {
		if err := stand.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("end the stand-in shim: %v", err)
		}
		<-exited
	})

	if err := vzvm.EndShim("a", vz.Open(filepath.Join(dir, "shim.sock")), stand.Process.Pid); err != nil {
		t.Fatalf("EndShim over a shim past its socket: %v", err)
	}
	if _, err := vz.Identify(stand.Process.Pid); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("EndShim returned while the shim still ran: %v", err)
	}
}

// fillQueue dials a socket nothing accepts until the kernel refuses, as the bounded execs on a frozen shim did.
func fillQueue(t *testing.T, socket string) {
	t.Helper()
	for range 512 {
		conn, err := net.Dial("unix", socket)
		if errors.Is(err, syscall.ECONNREFUSED) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("%s still takes dials after 512", socket)
}

// An output log a daemon before the bound left past it is bounded at the next daemon start, with no later output (SHARD-352).
func TestBoundOutputLogBoundsALegacyLogWithNoLaterOutput(t *testing.T) {
	h := newHarness(t)
	dir, err := h.stateDir("sb-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "output.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 11)), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := h.provider.BoundOutputLog("sb-legacy", 10); err != nil {
		t.Fatalf("BoundOutputLog: %v", err)
	}

	for name, want := range map[string]int64{path: 0, path + ".1": 10} {
		info, err := os.Stat(name)
		if err != nil || info.Size() != want {
			t.Errorf("%s: %v, want %d bytes", filepath.Base(name), err, want)
		}
	}
}

// vz finishes a cut pause's stage on the next resume, so AdoptStaging keeps dir+".tmp" and never drops it (SHARD-404).
func TestAdoptStagingKeepsACutPauseStage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	tmp := dir + ".tmp"
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatalf("stage a cut pause: %v", err)
	}

	if err := (&vzvm.Provider{}).AdoptStaging(dir); err != nil {
		t.Fatalf("AdoptStaging: %v", err)
	}

	if _, err := os.Stat(tmp); err != nil {
		t.Errorf("the staging %s is gone after adopt, want vz to keep it to finish on resume: %v", tmp, err)
	}
}

// A restore whose checkpoint disk is missing must leave the live disk in place, so a failed copy never bricks a sandbox (SHARD-589).
func TestRestoreDiskKeepsTheLiveDiskWhenTheCopyFails(t *testing.T) {
	stateDir, checkpoint := t.TempDir(), t.TempDir()
	disk := filepath.Join(stateDir, vzvm.DiskFile)
	if err := os.WriteFile(disk, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The checkpoint has no disk, so the copy fails and the swap never runs.
	if err := vzvm.RestoreDisk("sb-1", checkpoint, disk); err == nil {
		t.Fatal("restoreDisk with no checkpoint disk = nil, want an error")
	}
	got, err := os.ReadFile(disk)
	if err != nil {
		t.Fatalf("the live disk after a failed restore: %v, want it kept", err)
	}
	if string(got) != "live" {
		t.Errorf("the live disk = %q, want it unchanged", got)
	}
}
