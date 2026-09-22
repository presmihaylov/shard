package firecracker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	fcapi "github.com/presmihaylov/shard/pkg/firecracker"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/conformance"
	"github.com/presmihaylov/shard/services/provider/firecracker"
	"github.com/presmihaylov/shard/services/supervisor"
)

const stopGrace = 5 * time.Second

// harness is one provider over one short root: a unix socket path is 104 bytes at most, and t.TempDir is longer.
type harness struct {
	provider *firecracker.Provider
	root     string
	erofs    string

	next atomic.Int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	root, err := os.MkdirTemp("", "fc") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	// The fake vmm opens the image the way firecracker does, and never mounts it, so any file is an image.
	erofs := filepath.Join(root, "base.erofs")
	if err := os.WriteFile(erofs, []byte("erofs"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &harness{root: root, erofs: erofs}
	h.open(t)

	return h
}

// open is a daemon start: a provider over the root, which holds nothing of an earlier one in memory.
func (h *harness) open(t *testing.T) *firecracker.Provider {
	t.Helper()

	p, err := firecracker.New(firecracker.Config{
		Binary: os.Args[0],
		Kernel: "kernel",
		Init:   initBinary,
		Dir:    h.root,
		Dirs:   h.stateDir,
	})
	if err != nil {
		t.Fatalf("open the provider: %v", err)
	}
	// A boot bounds its vmm on the host cgroup, and a test host has no cgroup hierarchy to bound it on.
	p.SetCgroupRoot("")
	// The newest provider holds the live vmms, so a spec's cleanup must stop through it.
	h.provider = p

	return p
}

// reopen is a daemon restart: the first provider lets go of its vmms, and a second one adopts them.
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
		// Best effort: a subtest may have stopped and removed this one already, and its errors say nothing new.
		ctx := context.Background()
		h.provider.Stop(ctx, id, stopGrace)
		h.provider.Remove(ctx, id)
	})

	return models.SandboxSpec{ID: id, StateDir: dir, BaseDisk: h.erofs, Entrypoint: entrypoint, Resources: models.Resources{MemoryMiB: 256, DiskMiB: 16}}
}

// requireReflink skips where the root shares no blocks: Clone is refused there, and the suite would prove only the refusal.
func requireReflink(t *testing.T, root string) {
	t.Helper()

	probe := filepath.Join(root, "reflink-probe")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := bundle.Reflink(probe, probe+"-clone")
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skipf("%s shares no blocks, so Clone is refused there; put TMPDIR on xfs or btrfs: %v", root, err)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestConformance(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)

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

// The guest's cmdline names /dev/vda and /dev/vdb, so the drives go in as the image first, read-only, and the overlay second.
func TestCreateBootsTheImageUnderTheOverlay(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "exit 0")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}

	blob, err := os.ReadFile(filepath.Join(spec.StateDir, bootFile))
	if err != nil {
		t.Fatal(err)
	}
	var b boot
	if err := json.Unmarshal(blob, &b); err != nil {
		t.Fatal(err)
	}
	var source struct {
		Kernel string `json:"kernel_image_path"`
		Initrd string `json:"initrd_path"`
		Args   string `json:"boot_args"`
	}
	if err := json.Unmarshal(b.Source, &source); err != nil {
		t.Fatal(err)
	}
	if source.Kernel != "kernel" || source.Initrd != filepath.Join(h.root, "initrd.cpio") {
		t.Errorf("the boot source = %+v, want the kernel and the provider's initrd", source)
	}
	for _, want := range []string{"console=ttyS0", "-transport vsock", "-base /dev/vda", "-overlay /dev/vdb"} {
		if !strings.Contains(source.Args, want) {
			t.Errorf("the boot args %q lack %q", source.Args, want)
		}
	}
	wantDrives := []string{
		`{"drive_id":"base","path_on_host":"` + h.erofs + `","is_root_device":false,"is_read_only":true}`,
		`{"drive_id":"overlay","path_on_host":"` + filepath.Join(spec.StateDir, "overlay.raw") + `","is_root_device":false,"is_read_only":false}`,
	}
	var drives []string
	for _, d := range b.Drives {
		drives = append(drives, string(d))
	}
	if !slices.Equal(drives, wantDrives) {
		t.Errorf("the drives = %q, want %q", drives, wantDrives)
	}
}

func TestCreateRefusesAnImageWithoutAnErofsImage(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "exit 0")
	spec.BaseDisk = ""

	err := h.provider.Create(t.Context(), spec)
	if err == nil || !strings.Contains(err.Error(), spec.ID) || !strings.Contains(err.Error(), "erofs") {
		t.Fatalf("Create = %v, want a refusal that names the sandbox and the image", err)
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
	for _, want := range []string{spec.ID, "provider firecracker", "--memory 0", "--memory <MiB>", "128"} {
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

// The orchestrator asks before it writes a record, so a refused --memory or --cpus leaves no failed sandbox in ls.
func TestCheckResourcesRefusesWhatCreateRefuses(t *testing.T) {
	h := newHarness(t)

	for _, res := range []models.Resources{{MemoryMiB: 0}, {MemoryMiB: 64}} {
		err := h.provider.CheckResources(res)
		if err == nil || !strings.Contains(err.Error(), "128") {
			t.Fatalf("CheckResources(%+v) = %v, want a refusal that names the minimum", res, err)
		}
	}
	err := h.provider.CheckResources(models.Resources{MemoryMiB: 128, VCPUs: 33})
	if err == nil || !strings.Contains(err.Error(), "32") {
		t.Fatalf("CheckResources(33 vcpus) = %v, want a refusal that names the most", err)
	}
	if err := h.provider.CheckResources(models.Resources{MemoryMiB: 128, VCPUs: 32}); err != nil {
		t.Fatalf("CheckResources(128, 32) = %v, want nil", err)
	}
}

// An image with no PATH gets the OCI default, as the bundle gives it on Linux, so a named entrypoint resolves in the guest.
func TestCreateRecordsTheImageAndTheDefaultPath(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "sh", "-c", "exit 0")
	spec.Env = []string{"HOME=/root"}

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	r := readVM(t, spec.StateDir)
	if r.BaseDisk != h.erofs {
		t.Errorf("the record's image = %q, want %q", r.BaseDisk, h.erofs)
	}
	want := []string{"HOME=/root", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	if !slices.Equal(r.Run.Env, want) {
		t.Errorf("the record's env = %q, want %q", r.Run.Env, want)
	}
}

// A stopped sandbox starts again over the overlay the stop kept, and a wait then answers the new run.
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

// runLong creates and starts a sandbox that runs until it is stopped, and returns the pid of its vmm.
func (h *harness) runLong(t *testing.T) (models.SandboxSpec, int) {
	t.Helper()

	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning || status.PID == 0 {
		t.Fatalf("Status after Start = %+v, %v, want running with a pid", status, err)
	}

	return spec, status.PID
}

// A control stream the transport drops is dialed again while the VM runs: the sandbox stays running and the stop still reaches the guest.
func TestADroppedControlStreamIsDialedAgain(t *testing.T) {
	h := newHarness(t)
	spec, pid := h.runLong(t)
	if err := syscall.Kill(pid, syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status after the drop = %+v, %v, want running", status, err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop over the stream dialed again: %v", err)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after Stop = %+v, %v, want stopped", status, err)
	}
}

// A guest that no longer answers on its control port is still a running VM: Status says so, and Stop kills the vmm instead of waiting out a grace the guest cannot hear.
func TestStopKillsAVMWhoseGuestNoLongerAnswers(t *testing.T) {
	h := newHarness(t)
	spec, pid := h.runLong(t)
	if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status with the guest out of reach = %+v, %v, want running", status, err)
	}
	began := time.Now()
	if err := h.provider.Stop(t.Context(), spec.ID, 20*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("Stop took %s: it waited a grace on a guest that could not hear it", took)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after Stop = %+v, %v, want stopped", status, err)
	}
}

// The orchestrator's clone spec carries no entrypoint, so the clone runs the source's, on the source's image.
func TestCloneRunsTheSourceEntrypointFromASpecWithoutOne(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
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
	if !reflect.DeepEqual(got.Run, src.Run) || got.RootFS != src.RootFS || got.BaseDisk != src.BaseDisk {
		t.Errorf("the clone's record = %+v, want the source's run, rootfs and image %+v", got, src)
	}
	if _, err := os.Stat(filepath.Join(clone.StateDir, "overlay.raw")); err != nil {
		t.Errorf("the clone has no overlay of its own: %v", err)
	}
}

// vm is the part of the record the tests compare, decoded from the file as the provider wrote it.
type vm struct {
	BaseDisk string             `json:"base_disk"`
	RootFS   string             `json:"rootfs"`
	Run      supervisor.RunSpec `json:"run"`
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

// A host with /dev/kvm has all three snapshot verbs: the vmm writes the snapshot and loads it back.
func TestCapabilitiesAreTheThreeSnapshotVerbs(t *testing.T) {
	h := newHarness(t)
	want := models.Capabilities{Pause: true, Resume: true, Fork: true}
	if caps := h.provider.Capabilities(); caps != want {
		t.Fatalf("Capabilities = %+v, want %+v", caps, want)
	}
}

// A pause writes the whole snapshot and ends the VM; the marker goes in last, and nothing of the staging is left.
func TestPauseWritesTheSnapshotAndEndsTheVM(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec, _ := h.runLong(t)
	dir := t.TempDir()

	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	for _, name := range []string{"vmstate", "memory", "overlay.raw", "snapshot.json", "checkpoint.img"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s in the snapshot: %v, want it written", name, err)
		}
	}
	if _, err := os.Stat(dir + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging directory after Pause: %v, want gone", err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after Pause = %+v, %v, want stopped", status, err)
	}
}

// A resume brings the sandbox back over its own copy of the overlay and a link to the memory the snapshot keeps.
func TestResumeBringsTheSandboxBackOverTheSnapshot(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec, _ := h.runLong(t)
	dir := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	if err := h.provider.Resume(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || !status.Alive() {
		t.Fatalf("Status after Resume = %+v, %v, want alive", status, err)
	}
	memory := filepath.Join(spec.StateDir, "memory")
	if got := links(t, memory); got != 2 {
		t.Errorf("the memory has %d links, want 2: the sandbox maps the snapshot's own file", got)
	}
	if got := driveOf(t, spec.StateDir, "overlay"); got != filepath.Join(spec.StateDir, "overlay.raw") {
		t.Errorf("the overlay drive after Resume = %q, want the sandbox's own", got)
	}

	// The snapshot is not consumed: a stopped sandbox comes back from the same one.
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("the second Resume from the same snapshot: %v", err)
	}
	if err := h.provider.Remove(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(memory); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the memory after Remove: %v, want gone", err)
	}
	if got := links(t, filepath.Join(dir, "memory")); got != 1 {
		t.Errorf("the snapshot's memory has %d links after Remove, want 1", got)
	}
}

// One snapshot forks as many sandboxes as are asked of it: each takes its own overlay, and the snapshot stays whole.
func TestForkTakesACopyAndLeavesTheSnapshot(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec, _ := h.runLong(t)
	dir := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	src := readVM(t, spec.StateDir)

	forks := []models.SandboxSpec{h.forkSpec(t), h.forkSpec(t)}
	for _, fork := range forks {
		if err := h.provider.Fork(t.Context(), dir, fork); err != nil {
			t.Fatalf("Fork: %v", err)
		}
	}
	for _, fork := range forks {
		status, err := h.provider.Status(t.Context(), fork.ID)
		if err != nil || !status.Alive() {
			t.Fatalf("Status of fork %s = %+v, %v, want alive", fork.ID, status, err)
		}
		got := readVM(t, fork.StateDir)
		if got.BaseDisk != src.BaseDisk || got.RootFS != src.RootFS || !reflect.DeepEqual(got.Run, src.Run) {
			t.Errorf("the fork's record = %+v, want the snapshot's image, rootfs and run %+v", got, src)
		}
		if drive := driveOf(t, fork.StateDir, "overlay"); drive != filepath.Join(fork.StateDir, "overlay.raw") {
			t.Errorf("the fork's overlay drive = %q, want its own", drive)
		}
	}
	// The one memory file carries a link for each fork, so no fork copied it.
	if got := links(t, filepath.Join(dir, "memory")); got != 1+len(forks) {
		t.Errorf("the snapshot's memory has %d links, want %d", got, 1+len(forks))
	}
	for _, name := range []string{"vmstate", "memory", "checkpoint.img"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s after the forks: %v, want the snapshot whole", name, err)
		}
	}
}

// Each verb refuses the state it cannot take, and says which sandbox and which state that is.
func TestTheSnapshotVerbsRefuseWhatTheyCannotTake(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec, _ := h.runLong(t)

	empty := t.TempDir()
	if err := h.provider.Resume(t.Context(), spec.ID, empty); err == nil || !strings.Contains(err.Error(), "no complete snapshot") {
		t.Errorf("Resume without a snapshot = %v, want the refusal", err)
	}
	if err := h.provider.Fork(t.Context(), empty, h.forkSpec(t)); err == nil || !strings.Contains(err.Error(), "no complete snapshot") {
		t.Errorf("Fork without a snapshot = %v, want the refusal", err)
	}

	dir := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err == nil || !strings.Contains(err.Error(), "pause takes a running sandbox") {
		t.Errorf("Pause of a sandbox whose VM is gone = %v, want the refusal", err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, dir); err == nil || !strings.Contains(err.Error(), "resume takes a paused sandbox") {
		t.Errorf("Resume of a live sandbox = %v, want the refusal", err)
	}
	// A fork onto a live sandbox would take the directory from under it.
	onto := models.SandboxSpec{ID: spec.ID, StateDir: spec.StateDir, Resources: spec.Resources}
	if err := h.provider.Fork(t.Context(), dir, onto); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Fork onto a live sandbox = %v, want the refusal", err)
	}
}

// A pause that cannot finish leaves the VM running and the last snapshot whole: the new one is staged beside it.
func TestAFailedPauseResumesTheVMAndKeepsTheLastSnapshot(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec, _ := h.runLong(t)
	dir := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// The vmm holds no handle on the overlay, so dropping it breaks the copy after the vCPUs have stopped.
	if err := os.Remove(filepath.Join(spec.StateDir, "overlay.raw")); err != nil {
		t.Fatal(err)
	}
	err := h.provider.Pause(t.Context(), spec.ID, dir)
	if err == nil || !strings.Contains(err.Error(), "copy the overlay") {
		t.Fatalf("Pause with no overlay = %v, want the copy to fail", err)
	}
	status, statusErr := h.provider.Status(t.Context(), spec.ID)
	if statusErr != nil || !status.Alive() {
		t.Fatalf("Status after the failed Pause = %+v, %v, want the VM still there", status, statusErr)
	}
	if _, err := os.Stat(dir + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging directory after the failed Pause: %v, want gone", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "checkpoint.img")); err != nil {
		t.Errorf("the last snapshot after the failed Pause: %v, want it whole", err)
	}
}

// A pause over a directory that already holds a snapshot puts the new one there in one step, and nothing of the old stays.
func TestASecondPauseReplacesTheWholeSnapshot(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec, _ := h.runLong(t)
	dir := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("the first Pause: %v", err)
	}
	// A file only the first snapshot has: it must go with it, not survive beside the second.
	if err := os.WriteFile(filepath.Join(dir, "stale"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("the second Pause: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	want := []string{"checkpoint.img", "memory", "overlay.raw", "snapshot.json", "vmstate"}
	if !slices.Equal(got, want) {
		t.Errorf("the snapshot directory holds %v, want the second snapshot alone %v", got, want)
	}
	if _, err := os.Stat(dir + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging directory after the second Pause: %v, want gone", err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Resume from the second snapshot: %v", err)
	}
}

// A daemon cut mid-pause leaves a paused VM whose guest answers nothing; the next daemon resumes it instead of waiting on it.
func TestAPausedVMLeftByACutPauseComesBack(t *testing.T) {
	h := newHarness(t)
	spec, _ := h.runLong(t)

	// The vCPUs are stopped and no snapshot was written: this is the pause of a daemon that died before it ended the vmm.
	client, _, err := fcapi.Adopt(filepath.Join(spec.StateDir, "firecracker.sock"), filepath.Join(spec.StateDir, "vsock.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Pause(); err != nil {
		t.Fatal(err)
	}
	p := h.reopen(t)

	status, err := p.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status of the paused leftover = %+v, %v, want the sandbox running again", status, err)
	}
	if err := p.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop after the leftover came back: %v", err)
	}
}

// forkSpec is what the orchestrator hands Fork: an id, a directory and the bounds, and no entrypoint.
func (h *harness) forkSpec(t *testing.T) models.SandboxSpec {
	t.Helper()

	spec := h.newSpec(t)

	return models.SandboxSpec{ID: spec.ID, StateDir: spec.StateDir, Resources: spec.Resources}
}

// links is how many names the file has, which is what proves the memory is shared and not copied.
func links(t *testing.T, path string) int {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no link count for %s", path)
	}

	return int(stat.Nlink)
}

// driveOf is the host file behind one of the guest's drives, as the vmm last had it.
func driveOf(t *testing.T, dir, id string) string {
	t.Helper()

	blob, err := os.ReadFile(filepath.Join(dir, bootFile))
	if err != nil {
		t.Fatal(err)
	}
	var b boot
	if err := json.Unmarshal(blob, &b); err != nil {
		t.Fatal(err)
	}
	for _, raw := range b.Drives {
		var d struct {
			ID   string `json:"drive_id"`
			Path string `json:"path_on_host"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		if d.ID == id {
			return d.Path
		}
	}
	t.Fatalf("no drive %q under %s", id, dir)

	return ""
}

// A remove leaves the state directory with nothing of the VM in it; the directory itself is the repository's.
func TestRemoveDropsTheOverlayTheRecordAndTheSockets(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	if err := h.provider.Remove(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"overlay.raw", "vm.json", "firecracker.sock", "vsock.sock"} {
		if _, err := os.Stat(filepath.Join(spec.StateDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after Remove: %v, want gone", name, err)
		}
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.Exists {
		t.Fatalf("Status after Remove = %+v, %v, want none", status, err)
	}
}

// An exit the loop could not land is an error on every read, not a wait that never ends.
func TestALostExitSurfacesInsteadOfAnEndlessWait(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "exit 3")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	// A link into a directory that is not there refuses the exit file to root as well, where a mode bit would not.
	if err := os.Symlink(filepath.Join(spec.StateDir, "missing", "exit.json"), filepath.Join(spec.StateDir, "exit.json")); err != nil {
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
