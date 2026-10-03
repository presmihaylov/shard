package vzvm_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
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
	"github.com/presmihaylov/shard/pkg/vz"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/conformance"
	"github.com/presmihaylov/shard/services/provider/vzvm"
	"github.com/presmihaylov/shard/services/supervisor"
)

const stopGrace = 5 * time.Second

// harness is one provider over one short root: a unix socket path is 104 bytes at most, and t.TempDir is longer.
type harness struct {
	provider    *vzvm.Provider
	root        string
	disk        string
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

	h := &harness{root: root, disk: baseDisk(t, root), saveRestore: saveRestore, log: &safeBuffer{}}
	h.open(t)

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
		SnapshotDir: func(t *testing.T) string { return t.TempDir() },
		Shell:       func(script string) []string { return []string{"/bin/sh", "-c", script} },
		// The fake guest is a host process, so the suite writes under the root; a clone here proves the verbs and not the disk.
		Scratch: h.root,
		Reopen:  h.reopen,
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

// The bound needs room under the 32 MiB headroom, so a VM too small for one is refused by name, on a create and a clone.
func TestCreateRefusesAMemoryBoundBelowTheMinimum(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "exit 0")
	spec.Resources.MemoryMiB = 64

	err := h.provider.Create(t.Context(), spec)
	if err == nil || !strings.Contains(err.Error(), spec.ID) || !strings.Contains(err.Error(), "128 MiB") {
		t.Fatalf("Create = %v, want a refusal that names the sandbox and the minimum", err)
	}

	// Zero is unbounded on Linux; a VM has no unbounded memory, so the refusal names the provider and the flag instead of a default.
	spec.Resources.MemoryMiB = 0
	err = h.provider.Create(t.Context(), spec)
	for _, want := range []string{spec.ID, "provider vz", "--memory 0", "--memory <MiB>", "128"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Create with --memory 0 = %v, want %q named", err, want)
		}
	}

	source := h.newSpec(t, "/bin/sh", "-c", "exit 0")
	if err := h.provider.Create(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Stop(t.Context(), source.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
	clone := h.newSpec(t)
	clone = models.SandboxSpec{ID: clone.ID, StateDir: clone.StateDir, Resources: models.Resources{MemoryMiB: 64}}
	err = h.provider.Clone(t.Context(), source.ID, clone)
	if err == nil || !strings.Contains(err.Error(), clone.ID) || !strings.Contains(err.Error(), "128 MiB") {
		t.Fatalf("Clone = %v, want a refusal that names the sandbox and the minimum", err)
	}
	clone.Resources.MemoryMiB = 0
	err = h.provider.Clone(t.Context(), source.ID, clone)
	if err == nil || !strings.Contains(err.Error(), "--memory 0") {
		t.Fatalf("Clone with --memory 0 = %v, want the refusal by name", err)
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
	if err == nil || !strings.Contains(err.Error(), "use 128 or 131 MiB") {
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

// A clone boots from the disk alone, so a pause freezes the guest's root before it stops the VM, and every path that runs the guest again thaws it (SHARD-296).
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
	if err := h.provider.Fork(t.Context(), snap, fork); err != nil {
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

	// Without a disk to copy the snapshot cannot complete, and the pause gives the guest back able to write.
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

// The orchestrator's clone spec carries no entrypoint, so the clone runs the source's, from a stopped source and from a paused one.
func TestCloneRunsTheSourceEntrypointFromASpecWithoutOne(t *testing.T) {
	h := newHarness(t)
	source := h.newSpec(t, "/bin/sh", "-c", "exit 3")
	source.Env = []string{"KEPT=1"}
	if err := h.provider.Create(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), source.ID); err != nil {
		t.Fatal(err)
	}
	if exit, err := h.provider.Wait(t.Context(), source.ID); err != nil || exit.Code != 3 {
		t.Fatalf("Wait on the source = %+v, %v", exit, err)
	}
	if err := h.provider.Stop(t.Context(), source.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
	src := readVM(t, source.StateDir)

	clone := h.newSpec(t)
	clone = models.SandboxSpec{ID: clone.ID, StateDir: clone.StateDir, Resources: clone.Resources}
	if err := h.provider.Clone(t.Context(), source.ID, clone); err != nil {
		t.Fatalf("Clone from a stopped source: %v", err)
	}
	if exit, err := h.provider.Wait(t.Context(), clone.ID); err != nil || exit.Code != 3 {
		t.Fatalf("Wait on the clone = %+v, %v", exit, err)
	}
	got := readVM(t, clone.StateDir)
	if !reflect.DeepEqual(got.Run, src.Run) || got.RootFS != src.RootFS {
		t.Errorf("the clone's record runs %+v over %q, want the source's %+v over %q", got.Run, got.RootFS, src.Run, src.RootFS)
	}
	if got.MachineID == "" || got.MachineID == src.MachineID {
		t.Errorf("the clone's machine id is %q, want one of its own (the source's is %q)", got.MachineID, src.MachineID)
	}

	if err := h.provider.Start(t.Context(), source.ID); err != nil {
		t.Fatal(err)
	}
	snap := t.TempDir()
	if err := h.provider.Pause(t.Context(), source.ID, snap); err != nil {
		t.Fatal(err)
	}
	second := h.newSpec(t)
	second = models.SandboxSpec{ID: second.ID, StateDir: second.StateDir, Resources: second.Resources}
	if err := h.provider.Clone(t.Context(), source.ID, second); err != nil {
		t.Fatalf("Clone from a paused source: %v", err)
	}
	if exit, err := h.provider.Wait(t.Context(), second.ID); err != nil || exit.Code != 3 {
		t.Fatalf("Wait on the clone of a paused source = %+v, %v", exit, err)
	}
	if r := readVM(t, source.StateDir); !r.Paused {
		t.Errorf("the source's record after the clone = %+v, want it still paused", r)
	}
}

// vm is the part of the record the clone test compares, decoded from the file as the provider wrote it.
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

// A pause that crashed after its record leaves the staged snapshot beside the old one and a shim over a suspended guest; the daemon comes back, the service retries, and that pause is finished.
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

	// The crash state by hand: a complete second snapshot staged, a record that says paused, and the shim still up over a suspended guest.
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
	shim, _, err := vz.Adopt(t.Context(), filepath.Join(dir, "shim.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shim.Pause(); err != nil {
		t.Fatal(err)
	}

	// The daemon comes back and the service retries the pause, which finishes the crashed one: the staged snapshot goes in and the shim goes.
	again := h.open(t)
	if err := again.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	if _, err := shim.State(t.Context()); err == nil {
		t.Fatal("the shim the crashed pause left still answers")
	}
	if got := readJSON(t, filepath.Join(snap, "snapshot.json"))["pause"]; got != 2.0 {
		t.Fatalf("the snapshot in place is pause %v, want 2, the staged one", got)
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

	// An older staged snapshot, left by a swap whose cleanup failed, goes, and the one in place stays.
	if err := again.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(staged, os.DirFS(snap)); err != nil {
		t.Fatal(err)
	}
	setJSON(t, filepath.Join(staged, "snapshot.json"), "pause", 1)
	if err := again.Resume(t.Context(), spec.ID, snap); err != nil {
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
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			stopsAFrozenShim(t, restart)
		})
	}
}

func stopsAFrozenShim(t *testing.T, restart bool) {
	h := newHarness(t)
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
		for _, pid := range []int{-entrypoint, -guest, guest} {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Errorf("end the fake guest %d: %v", pid, err)
			}
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
		if err := syscall.Kill(-shim, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("end the frozen shim %d: %v", shim, err)
		}
	})
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

	began := time.Now()
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop with a frozen shim: %v", err)
	}
	if took := time.Since(began); took > stopGrace+10*time.Second {
		t.Errorf("Stop with a frozen shim took %s, want under the grace plus 10 s", took)
	}
	awaitExit(t, shim)
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.Alive() || status.State != models.StateStopped {
		t.Fatalf("Status after the stop = %+v, %v; want stopped", status, err)
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
