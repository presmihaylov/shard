package firecracker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	fcapi "github.com/presmihaylov/shard/pkg/firecracker"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/conformance"
	"github.com/presmihaylov/shard/services/provider/firecracker"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/supervisor"
)

const stopGrace = 5 * time.Second

// harness is one provider over one short root: a unix socket path is 104 bytes at most, and t.TempDir is longer.
type harness struct {
	provider *firecracker.Provider
	root     string
	erofs    string
	kernel   string

	next atomic.Int64

	mu sync.Mutex
	// owners is the uid each file was last given, by inode, which stands in for a chown a test cannot make.
	owners map[uint64]int
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
	kernel := filepath.Join(root, "kernel")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &harness{root: root, erofs: erofs, kernel: kernel, owners: map[uint64]int{}}
	h.open(t)

	return h
}

// open is a daemon start: a provider over the root, which holds nothing of an earlier one in memory.
func (h *harness) open(t *testing.T) *firecracker.Provider {
	t.Helper()

	p, err := firecracker.New(h.config())
	if err != nil {
		t.Fatalf("open the provider: %v", err)
	}
	p.SetOwners(h.own, func(string, string, int, int) error { return nil })
	// A boot bounds its vmm on the host cgroup, and a test host has no cgroup hierarchy to bound it on.
	p.SetCgroupRoot("")
	// The newest provider holds the live vmms, so a spec's cleanup must stop through it.
	h.provider = p

	return p
}

// config is what a daemon start hands the provider: this test binary, which answers as the jailer and the vmm.
func (h *harness) config() firecracker.Config {
	return firecracker.Config{
		Binary:    os.Args[0],
		Jailer:    os.Args[0],
		JailBase:  filepath.Join(h.root, "j"),
		Kernel:    h.kernel,
		Init:      initBinary,
		Dir:       h.root,
		Dirs:      h.stateDir,
		Snapshots: h.snapshotDir,
	}
}

// reopen is a daemon restart: the first provider lets go of its vmms, and a second one adopts them.
func (h *harness) reopen(t *testing.T) models.Provider {
	t.Helper()

	if err := h.provider.Close(); err != nil {
		t.Fatalf("close the provider: %v", err)
	}

	return h.open(t)
}

// own notes the uid a path was given.
func (h *harness) own(path string, uid, _ int) error {
	ino, err := inode(path)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.owners[ino] = uid

	return nil
}

// owner is the uid a file was last given, by inode so a rename keeps it, and -1 for one never given.
func (h *harness) owner(path string) int {
	ino, err := inode(path)
	if err != nil {
		return -1
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	uid, ok := h.owners[ino]
	if !ok {
		return -1
	}

	return uid
}

func inode(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("no inode for %s", path)
	}

	return st.Ino, nil
}

// jail is the chroot the jailer makes for the sandbox's vmm.
func (h *harness) jail(id string) string {
	return filepath.Join(h.root, "j", "firecracker", id, "root")
}

// api is the API socket of the sandbox's vmm, in its jail.
func (h *harness) api(id string) string {
	socket, _ := h.sockets(id)

	return socket
}

// sockets are the API and vsock sockets of the sandbox's vmm, in its jail.
func (h *harness) sockets(id string) (string, string) {
	paths := firecracker.JailSockets(filepath.Join(h.root, "j"), id)

	return paths[0], paths[1]
}

// stateDir answers for any id, as the repository does; only a spec's directory exists.
func (h *harness) stateDir(id string) (string, error) {
	return filepath.Join(h.root, "s", id), nil
}

// snapshotDir answers where a pause of id writes, as the repository does; nothing creates it before a pause.
func (h *harness) snapshotDir(id string) (string, error) {
	return filepath.Join(h.root, "snapshots", id), nil
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
	if source.Kernel != "/vmlinux" || source.Initrd != "/initrd" {
		t.Errorf("the boot source = %+v, want the kernel and the initrd in the jail", source)
	}
	for _, want := range []string{"console=ttyS0", "-transport vsock", "-base /dev/vda", "-overlay /dev/vdb"} {
		if !strings.Contains(source.Args, want) {
			t.Errorf("the boot args %q lack %q", source.Args, want)
		}
	}
	wantDrives := []string{
		`{"drive_id":"base","path_on_host":"/base.erofs","is_root_device":false,"is_read_only":true}`,
		`{"drive_id":"overlay","path_on_host":"/overlay.raw","is_root_device":false,"is_read_only":false,"cache_type":"Writeback"}`,
	}
	var drives []string
	for _, d := range b.Drives {
		drives = append(drives, string(d))
	}
	if !slices.Equal(drives, wantDrives) {
		t.Errorf("the drives = %q, want %q", drives, wantDrives)
	}
}

// A create spawns the vmm through the jailer, as a uid of its own, over files in its jail that only that uid can open (SHARD-306).
func TestCreatePutsTheVMMInAJailOfItsOwn(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "exit 0")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}

	jail := h.jail(spec.ID)
	args := readJailer(t, jail)
	r := readVM(t, spec.StateDir)
	if args.ID != spec.ID || args.UID < 0x70000000 || args.GID != args.UID || args.UID != r.UID {
		t.Errorf("the jailer ran with %+v, want the sandbox's id and the uid %d its record keeps, as the gid too", args, r.UID)
	}
	if args.ParentCgroup != "shard/"+spec.ID {
		t.Errorf("the parent cgroup = %q, want shard/%s with no leading slash", args.ParentCgroup, spec.ID)
	}
	if r.Jail != jail {
		t.Errorf("the record's jail = %q, want %q", r.Jail, jail)
	}
	for name, perm := range map[string]os.FileMode{"vmlinux": 0o400, "initrd": 0o400, "base.erofs": 0o400, "overlay.raw": 0o600} {
		path := filepath.Join(jail, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s in the jail: %v", name, err)
		}
		if info.Mode().Perm() != perm {
			t.Errorf("%s mode = %o, want %o", name, info.Mode().Perm(), perm)
		}
		if got := h.owner(path); got != r.UID {
			t.Errorf("%s went to uid %d, want the vmm's %d", name, got, r.UID)
		}
	}
	if !sameFile(t, filepath.Join(jail, "overlay.raw"), filepath.Join(spec.StateDir, "overlay.raw")) {
		t.Error("the jail's overlay is not the sandbox's own file")
	}
}

// readJailer is what the fake jailer was last run with for the jail.
func readJailer(t *testing.T, jail string) jailerArgs {
	t.Helper()

	blob, err := os.ReadFile(filepath.Join(filepath.Dir(jail), jailerFile))
	if err != nil {
		t.Fatal(err)
	}
	var args jailerArgs
	if err := json.Unmarshal(blob, &args); err != nil {
		t.Fatal(err)
	}

	return args
}

// No two sandboxes share a uid, so neither can open the other's jail; a restart keeps the uid, and a clone gets a new one.
func TestEverySandboxGetsAUIDOfItsOwn(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	first, _ := h.runLong(t)
	second, _ := h.runLong(t)
	uid := readVM(t, first.StateDir).UID
	if other := readVM(t, second.StateDir).UID; other == uid {
		t.Fatalf("two sandboxes share uid %d", uid)
	}

	if err := h.provider.Stop(t.Context(), first.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(h.jail(first.ID))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the jail after Stop: %v, want gone with the vmm", err)
	}
	if err := h.provider.Start(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	if got := readJailer(t, h.jail(first.ID)).UID; got != uid {
		t.Errorf("the vmm after a restart runs as %d, want the sandbox's %d", got, uid)
	}

	if err := h.provider.Stop(t.Context(), first.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
	clone := h.newSpec(t)
	clone = models.SandboxSpec{ID: clone.ID, StateDir: clone.StateDir, Resources: clone.Resources}
	if err := h.provider.Clone(t.Context(), first.ID, clone); err != nil {
		t.Fatal(err)
	}
	if got := readVM(t, clone.StateDir).UID; got == uid || got == readVM(t, second.StateDir).UID {
		t.Errorf("the clone got uid %d, which another sandbox has", got)
	}
}

// A uid counter past the range refuses the create, rather than hand out a uid another subsystem owns.
func TestCreateRefusesAUIDPastTheRange(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(filepath.Join(h.root, "next-uid"), []byte(strconv.Itoa(0x7FFE0000)), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := h.newSpec(t, "/bin/sh", "-c", "exit 0")

	err := h.provider.Create(t.Context(), spec)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("Create past the uid range = %v, want a refusal", err)
	}
}

// A snapshot from before the jail names host paths a jailed vmm cannot open, so a restore refuses it.
func TestARestoreRefusesASnapshotFromBeforeTheJail(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec, _ := h.runLong(t)
	dir := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	meta := filepath.Join(dir, "snapshot.json")
	blob, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(blob, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "jailed")
	if blob, err = json.Marshal(fields); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	err = h.provider.Resume(t.Context(), spec.ID, dir)
	if err == nil || !strings.Contains(err.Error(), "before the jail") {
		t.Fatalf("Resume from a snapshot before the jail = %v, want a refusal", err)
	}
	err = h.provider.ForkSnapshot(t.Context(), dir, h.forkSpec(t))
	if err == nil || !strings.Contains(err.Error(), "before the jail") {
		t.Fatalf("Fork from a snapshot before the jail = %v, want a refusal", err)
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
	for _, want := range []string{spec.ID, "provider firecracker", "--memory 0", "--memory 128MiB"} {
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
	for disk, want := range map[int64]string{1: "at least 11 MiB of disk", 10: "at least 11 MiB of disk", 129: "use 128 or 131 MiB"} {
		err := h.provider.CheckResources(models.Resources{MemoryMiB: 128, DiskMiB: disk})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("CheckResources(--disk %d) = %v, want %q", disk, err, want)
		}
	}
	if err := h.provider.CheckResources(models.Resources{MemoryMiB: 128, DiskMiB: bundle.MinOverlayDiskMiB}); err != nil {
		t.Fatalf("CheckResources(--disk %d) = %v, want nil", bundle.MinOverlayDiskMiB, err)
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

// A guest that floods every control stream is dialed a few times a second at most, and exec and stop still answer (SHARD-408).
func TestAGuestThatFloodsEveryControlStreamIsDialedAFewTimesASecondAtMost(t *testing.T) {
	h := newHarness(t)
	spec, pid := h.runLong(t)
	for _, marker := range []string{dialsFile, floodEveryFile} {
		if err := os.WriteFile(filepath.Join(spec.StateDir, marker), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Kill(pid, syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}

	const flood = 3 * time.Second
	time.Sleep(flood)
	dials, err := os.ReadFile(filepath.Join(spec.StateDir, dialsFile))
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
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop during the flood: %v", err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after the stop = %+v, %v; want stopped", status, err)
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

// A stopped vmm holds the control stream open and answers nothing: Stop waits its grace, not forever, then kills it (SHARD-339).
func TestStopKillsAVMThatHoldsTheStreamAndNeverAnswers(t *testing.T) {
	h := newHarness(t)
	spec, pid := h.runLong(t)
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}

	began := time.Now()
	if err := h.provider.Stop(t.Context(), spec.ID, time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("Stop took %s on a grace of 1s", took)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after Stop = %+v, %v, want stopped", status, err)
	}
}

// A vmm that freezes after its guest answered the stop costs the grace, not a state read's callTimeout (SHARD-388).
func TestStopEndsOnTimeWhenTheVMMFreezesAfterTheGuestAnswers(t *testing.T) {
	h := newHarness(t)
	pidFile := filepath.Join(t.TempDir(), "vmm.pid")
	// TERM reaches the entrypoint after the guest answered the stop; the sleep lets that answer cross the vmm before it freezes.
	script := fmt.Sprintf("trap 'sleep 0.3; kill -STOP $(cat %s); while true; do sleep 0.1; done' TERM; echo trapped; while true; do sleep 0.1; done", pidFile)
	spec := h.newSpec(t, "/bin/sh", "-c", script)
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
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(status.PID)), 0o600); err != nil {
		t.Fatal(err)
	}
	// A TERM before the trap is set ends the entrypoint, and the stop with it, before the vmm freezes.
	awaitLog(t, h.provider, spec.ID, 0)

	began := time.Now()
	if err := h.provider.Stop(t.Context(), spec.ID, 3*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if took := time.Since(began); took > 8*time.Second {
		t.Fatalf("Stop took %s on a grace of 3s", took)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after Stop = %+v, %v, want stopped", status, err)
	}
}

// frozenAfterARestart is a running vmm frozen while no daemon holds it, so the new daemon meets it only by its socket.
func (h *harness) frozenAfterARestart(t *testing.T) (models.SandboxSpec, int) {
	t.Helper()

	spec, pid := h.runLong(t)
	h.reopen(t)
	freezeVMM(t, pid)

	return spec, pid
}

// awaitReaped proves the kill reached the vmm, not only that a read said stopped.
func awaitReaped(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(stopGrace)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the vmm %d still exists %s after it read stopped: %v", pid, stopGrace, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A new daemon reads a vmm too frozen to answer unresponsive inside the serve bound, keeps it, and gives it back once it thaws (SHARD-392).
func TestStatusAfterARestartKeepsAFrozenVMMUnresponsive(t *testing.T) {
	h := newHarness(t)
	spec, pid := h.frozenAfterARestart(t)

	status := h.unresponsive(t, spec.ID, pid)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the vmm %d is gone after a read: %v", pid, err)
	}
	_, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "exit 0"}})
	if err == nil || !strings.Contains(err.Error(), status.Reason) {
		t.Fatalf("Exec on an unresponsive sandbox = %v, want a refusal that names %q", err, status.Reason)
	}

	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err = h.provider.Status(t.Context(), spec.ID)
		if err == nil && status.State == models.StateRunning && status.PID == pid {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("Status after the thaw = %+v, %v, want running on pid %d", status, err, pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
	exit, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "exit 7"}})
	if err != nil || exit.Code != 7 {
		t.Fatalf("Exec after the thaw = %+v, %v, want exit 7", exit, err)
	}
}

// Lookups that meet a thawed vmm at once attach it once, so one control stream follows the guest.
func TestLookupsThatRaceAThawAttachTheVMMOnce(t *testing.T) {
	h := newHarness(t)
	spec, pid := h.frozenAfterARestart(t)
	h.unresponsive(t, spec.ID, pid)
	watchControls(t, spec)

	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	const lookups = 8
	errs := make(chan error, lookups)
	for range lookups {
		go func() {
			status, err := h.provider.Status(t.Context(), spec.ID)
			if err == nil && status.State != models.StateRunning {
				err = fmt.Errorf("status %+v, want running", status)
			}
			errs <- err
		}()
	}
	for range lookups {
		if err := <-errs; err != nil {
			t.Fatalf("a lookup that raced the thaw: %v", err)
		}
	}
	if opened := len(controls(t, spec.StateDir, "attach")); opened != 1 {
		t.Fatalf("the host opened %d control streams to one thawed vmm, want 1", opened)
	}
}

// A stop after the daemon read the vmm unresponsive kills it by its pid in a short re-probe, not a grace or callTimeout (SHARD-392).
func TestStopAfterARestartKillsAFrozenVMMReadUnresponsive(t *testing.T) {
	h := newHarness(t)
	spec, pid := h.frozenAfterARestart(t)
	h.unresponsive(t, spec.ID, pid)

	began := time.Now()
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop after %s: %v", time.Since(began), err)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("Stop took %s on a vmm already read unresponsive", took)
	}
	awaitReaped(t, pid)
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after Stop = %+v, %v, want stopped", status, err)
	}
}

// A stop that is the first to meet a frozen vmm by its socket still ends it by the adopt bound (SHARD-392).
func TestStopAfterARestartKillsAFrozenVMMNoReadMet(t *testing.T) {
	h := newHarness(t)
	spec, pid := h.frozenAfterARestart(t)

	began := time.Now()
	if err := h.provider.Stop(t.Context(), spec.ID, time.Second); err != nil {
		t.Fatalf("Stop after %s: %v", time.Since(began), err)
	}
	if took := time.Since(began); took > 8*time.Second {
		t.Fatalf("Stop took %s on a grace of 1s", took)
	}
	awaitReaped(t, pid)
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after Stop = %+v, %v, want stopped", status, err)
	}
}

// A stop kills a silent vmm through the pin its adopt took, so a process that holds its pid number since is never hit (SHARD-392).
func TestStopOfASilentVMMNeverKillsTheProcessOnItsPidSince(t *testing.T) {
	h := newHarness(t)
	spec, pid := h.frozenAfterARestart(t)
	h.unresponsive(t, spec.ID, pid)
	innocent := exec.Command("sleep", "60")
	if err := innocent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := innocent.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		var exit *exec.ExitError
		if err := innocent.Wait(); err != nil && !errors.As(err, &exit) {
			t.Error(err)
		}
	})
	h.provider.RenumberSilent(spec.ID, innocent.Process.Pid)

	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	awaitReaped(t, pid)
	if err := syscall.Kill(innocent.Process.Pid, 0); err != nil {
		t.Fatalf("the process on the silent vmm's pid since was hit: %v", err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after Stop = %+v, %v, want stopped", status, err)
	}
}

// A vmm that freezes while this provider holds it reads unresponsive on its pid within the bound, and running again once it thaws (SHARD-439).
func TestAHeldVMMThatFreezesReadsUnresponsiveUntilItThaws(t *testing.T) {
	h := newHarness(t)
	spec, pid := h.runLong(t)
	freezeVMM(t, pid)

	h.unresponsive(t, spec.ID, pid)
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(stopGrace); ; time.Sleep(100 * time.Millisecond) {
		status, err := h.provider.Status(t.Context(), spec.ID)
		if err == nil && status.State == models.StateRunning && status.Reason == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Status %s after the thaw = %+v, %v, want running", stopGrace, status, err)
		}
	}
}

// Exec and pause on a held vmm that froze refuse within the bound and name it, instead of waiting on a guest that cannot answer (SHARD-439).
func TestExecAndPauseOnAFrozenHeldVMMRefuseWithinTheBound(t *testing.T) {
	h := newHarness(t)
	spec, pid := h.runLong(t)
	freezeVMM(t, pid)
	t.Cleanup(func() {
		if err := syscall.Kill(pid, syscall.SIGCONT); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Error(err)
		}
	})

	began := time.Now()
	_, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "exit 0"}})
	if err == nil || !strings.Contains(err.Error(), string(models.StateUnresponsive)) || !strings.Contains(err.Error(), strconv.Itoa(pid)) {
		t.Fatalf("Exec on a frozen vmm = %v, want a refusal that names it unresponsive on pid %d", err, pid)
	}
	if took := time.Since(began); took >= 8*time.Second {
		t.Fatalf("Exec took %s to refuse a frozen vmm", took)
	}
	err = h.provider.Pause(t.Context(), spec.ID, filepath.Join(h.root, "snap-"+spec.ID))
	if _, ok := errors.AsType[*models.UnresponsiveError](err); !ok {
		t.Fatalf("Pause of a frozen vmm = %v, want an UnresponsiveError", err)
	}
}

// A stop of a held vmm that froze kills it through the pin its attach took, so a process on its pid since lives, and the stop takes no grace (SHARD-439).
func TestStopOfAFrozenHeldVMMKillsThroughItsPinNotItsPid(t *testing.T) {
	h := newHarness(t)
	spec, pid := h.runLong(t)
	freezeVMM(t, pid)
	h.unresponsive(t, spec.ID, pid)
	innocent := exec.Command("sleep", "60")
	if err := innocent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := innocent.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		var exit *exec.ExitError
		if err := innocent.Wait(); err != nil && !errors.As(err, &exit) {
			t.Error(err)
		}
	})
	h.provider.RenumberSilent(spec.ID, innocent.Process.Pid)

	began := time.Now()
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("Stop took %s on a vmm already read unresponsive", took)
	}
	awaitReaped(t, pid)
	if err := syscall.Kill(innocent.Process.Pid, 0); err != nil {
		t.Fatalf("the process on the frozen vmm's pid since was hit: %v", err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after Stop = %+v, %v, want stopped", status, err)
	}
}

// freezeVMM stops a vmm with SIGSTOP, as a host under load or an operator can, and waits until every thread of it has stopped, which a kill does not.
func freezeVMM(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		done, err := stopped(pid)
		if err != nil {
			t.Fatalf("read the state of the vmm %d: %v", pid, err)
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the vmm %d did not stop within 5s", pid)
		}
	}
}

// unresponsive reads a frozen vmm the way a booting daemon does, and proves it reads unresponsive on its pid inside the serve bound.
func (h *harness) unresponsive(t *testing.T, id string, pid int) models.Status {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), sandbox.DefaultProbeBudget)
	defer cancel()
	began := time.Now()
	status, err := h.provider.Status(ctx, id)
	if err != nil || status.State != models.StateUnresponsive || status.PID != pid || !strings.Contains(status.Reason, strconv.Itoa(pid)) {
		t.Fatalf("Status after %s = %+v, %v, want unresponsive on pid %d", time.Since(began), status, err, pid)
	}
	if took := time.Since(began); took >= 5*time.Second {
		t.Fatalf("Status took %s on a frozen vmm, past the 5s a daemon has to serve", took)
	}

	return status
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
	UID      int                `json:"uid,omitempty"`
	Jail     string             `json:"jail,omitempty"`
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

// A host with /dev/kvm pauses and resumes; fork waits for the live fork of SHARD-462 (SHARD-457).
func TestCapabilitiesArePauseAndResume(t *testing.T) {
	h := newHarness(t)
	want := models.Capabilities{Pause: true, Resume: true}
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

// The vmm writes vmstate and memory as its own uid and under its own umask, so Pause gives them to root and tightens them.
func TestPauseTightensTheSnapshotFiles(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec, _ := h.runLong(t)
	dir := t.TempDir()

	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	for _, name := range []string{"vmstate", "memory", "checkpoint.img"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, info.Mode().Perm())
		}
	}
	for _, name := range []string{"vmstate", "memory"} {
		if got := h.owner(filepath.Join(dir, name)); got != 0 {
			t.Errorf("%s went to uid %d, want root", name, got)
		}
	}
	if _, err := os.Stat(filepath.Dir(h.jail(spec.ID))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the jail after Pause: %v, want gone with the vmm", err)
	}
}

// A resume brings the sandbox back over its own copy of the overlay, with a copy by reference of the memory the snapshot keeps in its jail.
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
	requireJailed(t, h, spec, dir)

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
	if _, err := os.Stat(filepath.Dir(h.jail(spec.ID))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the jail after Remove: %v, want gone", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "memory")); err != nil {
		t.Errorf("the snapshot's memory after Remove: %v, want it kept", err)
	}
}

// requireJailed proves a restored vmm opens its own overlay through its jail, and maps a copy of the snapshot's memory the snapshot does not share a name with.
func requireJailed(t *testing.T, h *harness, spec models.SandboxSpec, snapshot string) {
	t.Helper()

	if got := driveOf(t, spec.StateDir, "overlay"); got != "/overlay.raw" {
		t.Errorf("the overlay drive = %q, want the one in the jail", got)
	}
	jail := h.jail(spec.ID)
	if !sameFile(t, filepath.Join(jail, "overlay.raw"), filepath.Join(spec.StateDir, "overlay.raw")) {
		t.Error("the jail's overlay is not the sandbox's own file, so the guest writes where clone and pause never read")
	}
	memory := filepath.Join(jail, "memory")
	if got := links(t, memory); got != 1 {
		t.Errorf("the jail's memory has %d links, want 1: a copy by reference, not the snapshot's file", got)
	}
	if got := h.owner(memory); got != readVM(t, spec.StateDir).UID {
		t.Errorf("the jail's memory went to uid %d, want the vmm's", got)
	}
	if got := links(t, filepath.Join(snapshot, "memory")); got != 1 {
		t.Errorf("the snapshot's memory has %d links, want 1: no vmm maps the snapshot's own file", got)
	}
	if _, err := os.Stat(filepath.Join(spec.StateDir, "memory")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the memory in the state directory: %v, want none", err)
	}
}

// sameFile says two paths name one file, which is what a hard link into the jail is.
func sameFile(t *testing.T, a, b string) bool {
	t.Helper()

	ai, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	bi, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}

	return os.SameFile(ai, bi)
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
		if err := h.provider.ForkSnapshot(t.Context(), dir, fork); err != nil {
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
		requireJailed(t, h, fork, dir)
	}
	for _, name := range []string{"vmstate", "memory", "checkpoint.img"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s after the forks: %v, want the snapshot whole", name, err)
		}
	}
}

// Every restore of one snapshot wakes with the same crng key, so the source's resume and each fork are reseeded once, and only on a restore.
func TestEveryRestoreReseedsTheGuest(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec, _ := h.runLong(t)
	forks := []models.SandboxSpec{h.forkSpec(t), h.forkSpec(t)}
	for _, s := range append([]models.SandboxSpec{spec}, forks...) {
		watchControls(t, s)
	}

	dir := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	for _, fork := range forks {
		if err := h.provider.ForkSnapshot(t.Context(), dir, fork); err != nil {
			t.Fatalf("Fork: %v", err)
		}
	}
	if err := h.provider.Resume(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	for _, s := range append([]models.SandboxSpec{spec}, forks...) {
		if got := controls(t, s.StateDir, supervisor.KindReseed); !slices.Equal(got, []string{supervisor.KindReseed}) {
			t.Errorf("the guest of %s read %q, want one reseed", s.ID, got)
		}
		if _, err := os.Stat(filepath.Join(s.StateDir, firecracker.ReseedFile)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the reseed marker of %s after the restore: %v, want it gone", s.ID, err)
		}
	}
}

// A daemon cut between a restore's attach and its reseed leaves the marker, so the next daemon reseeds the guest it adopts, and the one after does not again.
func TestADaemonCutBeforeTheReseedLeavesItToTheNext(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec, _ := h.runLong(t)
	fork := h.forkSpec(t)
	for _, s := range []models.SandboxSpec{spec, fork} {
		watchControls(t, s)
	}
	dir := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := h.provider.ForkSnapshot(t.Context(), dir, fork); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	for _, s := range []models.SandboxSpec{spec, fork} {
		if err := os.WriteFile(filepath.Join(s.StateDir, firecracker.ReseedFile), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for range 2 {
		p := h.reopen(t)
		for _, s := range []models.SandboxSpec{spec, fork} {
			status, err := p.Status(t.Context(), s.ID)
			if err != nil || !status.Alive() {
				t.Fatalf("Status of %s after the restart = %+v, %v, want it adopted", s.ID, status, err)
			}
		}
	}

	for _, s := range []models.SandboxSpec{spec, fork} {
		if got := controls(t, s.StateDir, supervisor.KindReseed); !slices.Equal(got, []string{supervisor.KindReseed, supervisor.KindReseed}) {
			t.Errorf("the guest of %s read %q, want the restore's reseed and the first adopter's", s.ID, got)
		}
		if _, err := os.Stat(filepath.Join(s.StateDir, firecracker.ReseedFile)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the reseed marker of %s after the adopt: %v, want it gone", s.ID, err)
		}
	}
}

// A pause freezes the guest before the snapshot, and every restore reseeds the frozen guest before the thaw lets it run on the saved key (SHARD-409).
func TestARestoreReseedsTheFrozenGuestBeforeTheThaw(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec, _ := h.runLong(t)
	fork := h.forkSpec(t)
	for _, s := range []models.SandboxSpec{spec, fork} {
		watchControls(t, s)
	}
	dir := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := h.provider.ForkSnapshot(t.Context(), dir, fork); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	kinds := []string{supervisor.KindFreeze, supervisor.KindReseed, supervisor.KindThaw}
	if got := controls(t, spec.StateDir, kinds...); !slices.Equal(got, kinds) {
		t.Errorf("the guest of the source read %q, want %q", got, kinds)
	}
	if got := controls(t, fork.StateDir, kinds...); !slices.Equal(got, kinds[1:]) {
		t.Errorf("the guest of the fork read %q, want %q", got, kinds[1:])
	}
}

// A guest that cannot hold its root refuses the pause: the VM runs on, the guest is told to thaw, and nothing is written (SHARD-409).
func TestAPauseTheGuestCannotFreezeForIsRefused(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec, _ := h.runLong(t)
	watchControls(t, spec)
	refuse := filepath.Join(spec.StateDir, refuseFreezeFile)
	if err := os.WriteFile(refuse, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	err := h.provider.Pause(t.Context(), spec.ID, dir)
	if err == nil || !strings.Contains(err.Error(), "freeze the guest before the pause") {
		t.Fatalf("Pause of a guest that refuses the freeze = %v, want the refusal", err)
	}
	status, statusErr := h.provider.Status(t.Context(), spec.ID)
	if statusErr != nil || status.State != models.StateRunning {
		t.Fatalf("Status after the refused Pause = %+v, %v, want running", status, statusErr)
	}
	for _, path := range []string{dir + ".tmp", filepath.Join(dir, "checkpoint.img")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after the refused Pause: %v, want none", path, err)
		}
	}
	if got := controls(t, spec.StateDir, supervisor.KindFreeze, supervisor.KindThaw); !slices.Equal(got, []string{supervisor.KindFreeze, supervisor.KindThaw}) {
		t.Errorf("the guest read %q, want the refused freeze and a thaw", got)
	}

	if err := os.Remove(refuse); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause once the guest takes the freeze: %v", err)
	}
}

// A VM booted before the guest froze its overlay root keeps that shard-init, so its pause is refused with the restart that fixes it, and no freeze reaches the guest (SHARD-409).
func TestAPauseOfAGuestFromBeforeTheFreezeIsRefused(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec, _ := h.runLong(t)
	watchControls(t, spec)
	old := filepath.Join(spec.StateDir, oldGuestFile)
	if err := os.WriteFile(old, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The daemon of an upgrade attaches to a VM the one before it booted.
	if err := h.provider.Close(); err != nil {
		t.Fatal(err)
	}
	p := h.open(t)
	dir := t.TempDir()

	err := p.Pause(t.Context(), spec.ID, dir)
	if err == nil || !strings.Contains(err.Error(), "restart the sandbox, then pause it") {
		t.Fatalf("Pause of a guest from before the freeze = %v, want the refusal that names the restart", err)
	}
	status, statusErr := p.Status(t.Context(), spec.ID)
	if statusErr != nil || status.State != models.StateRunning {
		t.Fatalf("Status after the refused Pause = %+v, %v, want running", status, statusErr)
	}
	if _, err := os.Stat(dir + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s.tmp after the refused Pause: %v, want none", dir, err)
	}
	if got := controls(t, spec.StateDir, supervisor.KindFreeze, supervisor.KindThaw); len(got) != 0 {
		t.Errorf("the guest read %q, want no freeze sent to a guest that cannot take it", got)
	}

	if err := os.Remove(old); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause after the restart: %v", err)
	}
}

// A drop that takes the guest's answer to a freeze leaves it frozen, so the pause's undo or the stream dialed again thaws it, once, and the next pause goes through (SHARD-409).
func TestAFreezeWhoseAnswerADropTookIsThawed(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec, _ := h.runLong(t)
	watchControls(t, spec)
	if err := os.WriteFile(filepath.Join(spec.StateDir, loseFreezeFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	err := h.provider.Pause(t.Context(), spec.ID, dir)
	if err == nil || !strings.Contains(err.Error(), "freeze the guest before the pause") {
		t.Fatalf("Pause whose freeze lost its answer = %v, want the refusal", err)
	}
	// Which side thaws depends on whether the redial lands before the undo, so the thaw is waited for.
	want := []string{supervisor.KindFreeze, supervisor.KindThaw}
	got := controls(t, spec.StateDir, want...)
	for deadline := time.Now().Add(10 * time.Second); !slices.Equal(got, want) && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
		got = controls(t, spec.StateDir, want...)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the guest read %q, want the freeze and one thaw", got)
	}
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause after the thaw: %v", err)
	}
}

// A daemon cut between the freeze and the snapshot leaves the guest frozen, and the next daemon thaws it as it adopts the VM (SHARD-409).
func TestAGuestACutPauseLeftFrozenIsThawedByTheNextDaemon(t *testing.T) {
	h := newHarness(t)
	spec, _ := h.runLong(t)
	watchControls(t, spec)
	if err := h.provider.Close(); err != nil {
		t.Fatal(err)
	}

	// This is the pause of a daemon that froze the guest and stopped the vCPUs, then died before the snapshot.
	api, vsock := h.sockets(spec.ID)
	client, _, err := fcapi.Adopt(t.Context(), api, vsock)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := client.Connect(supervisor.ControlPort)
	if err != nil {
		t.Fatal(err)
	}
	control := supervisor.ControlOver(conn)
	if _, err := control.Next(); err != nil {
		t.Fatal(err)
	}
	if err := control.Freeze(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Pause(); err != nil {
		t.Fatal(err)
	}
	p := h.open(t)

	status, err := p.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status of the frozen leftover = %+v, %v, want the sandbox running again", status, err)
	}
	if got := controls(t, spec.StateDir, supervisor.KindFreeze, supervisor.KindThaw); !slices.Equal(got, []string{supervisor.KindFreeze, supervisor.KindThaw}) {
		t.Errorf("the guest read %q, want the cut pause's freeze and the next daemon's thaw", got)
	}
}

// A frozen guest the next daemon cannot reseed is ended, not left frozen for every later verb to fail on (SHARD-409).
func TestAnAdoptedFrozenGuestThatRefusesTheReseedIsEnded(t *testing.T) {
	h := newHarness(t)
	spec, _ := h.runLong(t)
	watchControls(t, spec)
	if err := h.provider.Close(); err != nil {
		t.Fatal(err)
	}

	// This is a restore's vmm whose daemon died before the reseed, over a guest that will refuse it.
	api, vsock := h.sockets(spec.ID)
	client, _, err := fcapi.Adopt(t.Context(), api, vsock)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := client.Connect(supervisor.ControlPort)
	if err != nil {
		t.Fatal(err)
	}
	control := supervisor.ControlOver(conn)
	if _, err := control.Next(); err != nil {
		t.Fatal(err)
	}
	if err := control.Freeze(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{firecracker.ReseedFile, refuseReseedFile} {
		if err := os.WriteFile(filepath.Join(spec.StateDir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := h.open(t)

	if _, err := p.Status(t.Context(), spec.ID); err == nil || !strings.Contains(err.Error(), "reseed the restored guest") {
		t.Fatalf("Status of a frozen guest that refuses the reseed = %v, want the refusal", err)
	}
	status, err := p.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after the refused reseed = %+v, %v, want the sandbox stopped", status, err)
	}
	if got := controls(t, spec.StateDir, supervisor.KindFreeze, supervisor.KindReseed, supervisor.KindThaw); !slices.Equal(got, []string{supervisor.KindFreeze, supervisor.KindReseed}) {
		t.Errorf("the guest read %q, want the freeze and the refused reseed, and no thaw", got)
	}
	if err := p.Remove(t.Context(), spec.ID); err != nil {
		t.Errorf("Remove after the refused reseed: %v", err)
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
	if err := h.provider.ForkSnapshot(t.Context(), empty, h.forkSpec(t)); err == nil || !strings.Contains(err.Error(), "no complete snapshot") {
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
	if err := h.provider.ForkSnapshot(t.Context(), dir, onto); err == nil || !strings.Contains(err.Error(), "already exists") {
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
	api, vsock := h.sockets(spec.ID)
	client, _, err := fcapi.Adopt(t.Context(), api, vsock)
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

// A daemon cut after a pause installed its snapshot leaves the guest frozen beside it; the next daemon ends that vmm and never runs the guest past it (SHARD-427).
func TestAVMFrozenBesideItsSnapshotIsEndedNotResumed(t *testing.T) {
	h := newHarness(t)
	requireReflink(t, h.root)
	spec, _ := h.runLong(t)
	dir, _ := h.snapshotDir(spec.ID)
	record := filepath.Join(spec.StateDir, "vm.json")
	shape, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Install(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// A daemon from before this fix left the record as the boot wrote it, and its leftover must be judged the same.
	if err := os.WriteFile(record, shape, 0o600); err != nil {
		t.Fatal(err)
	}
	p := h.reopen(t)

	status, err := p.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status of the frozen leftover = %+v, %v, want stopped", status, err)
	}
	if _, info, err := fcapi.Adopt(t.Context(), h.api(spec.ID), ""); err == nil {
		t.Fatalf("the frozen vmm still answers in state %s after the new daemon read it", info.State)
	}
	if err := p.Resume(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Resume from the snapshot the cut pause installed: %v", err)
	}
	status, err = p.Status(t.Context(), spec.ID)
	if err != nil || !status.Alive() {
		t.Fatalf("Status after Resume = %+v, %v, want alive", status, err)
	}
	if err := p.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop after Resume: %v", err)
	}
}

// A daemon cut mid-fork leaves a paused VM its load may still hold on the source's overlay; the marker makes the next daemon end it, never resume it onto the live source (SHARD-321).
func TestAPausedVMLeftByACutForkIsEndedNotResumed(t *testing.T) {
	h := newHarness(t)
	spec, _ := h.runLong(t)

	// The vCPUs are stopped as a cut fork leaves them, and the marker says the load may still point the overlay at the source.
	api, vsock := h.sockets(spec.ID)
	client, _, err := fcapi.Adopt(t.Context(), api, vsock)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Pause(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec.StateDir, firecracker.RestoringFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	p := h.reopen(t)
	status, err := p.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status of the half-forked leftover = %+v, %v, want stopped", status, err)
	}
	// The refuse ended the vmm, so nothing answers the socket as a live VM; a blind resume would have left it running on the source.
	if _, _, err := fcapi.Adopt(t.Context(), h.api(spec.ID), ""); err == nil {
		t.Fatal("the vmm a cut fork left still answers; the refuse must end it, not resume it")
	}
}

// A daemon cut between a fork's spawn and its load leaves a vmm with no guest; the next daemon ends it, so a remove frees the host (SHARD-295).
func TestAnUnloadedVMMLeftByACutForkIsEnded(t *testing.T) {
	h := newHarness(t)
	spec := h.forkSpec(t)
	exited := h.leaveUnloaded(t, spec, os.Args[0], h.jail(spec.ID))
	requireUnloadedEnded(t, h.reopen(t), spec, exited, h.jail(spec.ID))
}

// A vmm a daemon before the jail spawned answers in the state directory, and the next daemon still finds it there and ends it (SHARD-306).
func TestAVMMFromBeforeTheJailIsFoundAtItsOldSocket(t *testing.T) {
	h := newHarness(t)
	spec := h.forkSpec(t)
	exited := h.leaveUnloaded(t, spec, os.Args[0], "")
	requireUnloadedEnded(t, h.reopen(t), spec, exited, "")
}

// A read that lands mid-spawn is not a restart of the daemon, so the unloaded vmm is left to the spawn it belongs to.
func TestAVMMThisProcessStillSpawnsIsLeftToIt(t *testing.T) {
	h := newHarness(t)
	spec := h.forkSpec(t)
	h.leaveUnloaded(t, spec, os.Args[0], h.jail(spec.ID))
	done := h.provider.Spawning(spec.ID)
	defer done()

	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status mid-spawn = %+v, %v, want stopped", status, err)
	}
	if !unloaded(h.api(spec.ID)) {
		t.Fatal("a read ended the vmm a spawn in this process still brings up")
	}
}

// A read that saw the spawn's vmm "Not started" and resumes only after the attach leaves the sandbox it became running.
func TestAReadThatSawASpawnUnloadedSparesTheVMItBecame(t *testing.T) {
	h := newHarness(t)
	spec, pid := h.runLong(t)
	api, vsock := h.sockets(spec.ID)
	client, _, err := fcapi.Adopt(t.Context(), api, vsock)
	if err != nil {
		t.Fatal(err)
	}

	if err := h.provider.EndJudged(spec.ID, client, pid, h.jail(spec.ID)); err != nil {
		t.Fatalf("the late read: %v", err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning || status.PID != pid {
		t.Fatalf("Status after the late read = %+v, %v, want running on vmm %d", status, err, pid)
	}
}

// A read ends the unloaded vmm it saw, never one that answers the socket since.
func TestAReadEndsOnlyTheUnloadedVMMItSaw(t *testing.T) {
	h := newHarness(t)
	spec := h.forkSpec(t)
	exited := h.leaveUnloaded(t, spec, os.Args[0], h.jail(spec.ID))
	socket, vsock := h.sockets(spec.ID)
	client, info, err := fcapi.Adopt(t.Context(), socket, vsock)
	if err != nil {
		t.Fatal(err)
	}
	if err := fcapi.KillPID(info.PID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(stopGrace):
		t.Fatal("the first vmm still runs after its kill")
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	h.leaveUnloaded(t, spec, os.Args[0], h.jail(spec.ID))

	if err := h.provider.EndJudged(spec.ID, client, info.PID, h.jail(spec.ID)); err != nil {
		t.Fatalf("the late read: %v", err)
	}
	if !unloaded(socket) {
		t.Fatal("a read ended a vmm it never saw")
	}
}

// A read that timed out on a frozen vmm ends it by the pid it waited on, never a vmm that took the socket since (SHARD-392).
func TestAReadThatTimedOutEndsOnlyTheVMMItWaitedOn(t *testing.T) {
	h := newHarness(t)
	spec := h.forkSpec(t)
	exited := h.leaveUnloaded(t, spec, os.Args[0], h.jail(spec.ID))
	socket, vsock := h.sockets(spec.ID)
	_, frozen, err := fcapi.Adopt(t.Context(), socket, vsock)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(frozen.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, info, err := fcapi.Adopt(ctx, socket, vsock)
	if err == nil || info.PID != frozen.PID {
		t.Fatalf("Adopt of the frozen vmm = pid %d, %v, want a timeout that names pid %d", info.PID, err, frozen.PID)
	}
	// The frozen vmm exits and a spawn takes the socket between the timeout and the kill.
	if err := fcapi.KillPID(frozen.PID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(stopGrace):
		t.Fatal("the frozen vmm still runs after its kill")
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	h.leaveUnloaded(t, spec, os.Args[0], h.jail(spec.ID))

	if err := h.provider.EndJudged(spec.ID, fcapi.Open(socket, vsock), info.PID, h.jail(spec.ID)); err != nil {
		t.Fatalf("the late kill: %v", err)
	}
	if !unloaded(socket) {
		t.Fatal("a read that timed out ended a vmm that took the socket since")
	}
}

// leaveUnloaded writes the record a fork writes and spawns a vmm in jail that loads nothing, as a daemon cut between the two leaves them; the channel closes when the vmm exits.
// An empty jail is a vmm a daemon before the jail spawned, with its socket in the state directory.
func (h *harness) leaveUnloaded(t *testing.T, spec models.SandboxSpec, binary, jail string) <-chan struct{} {
	t.Helper()

	r := vm{BaseDisk: h.erofs, Jail: jail}
	socket := filepath.Join(spec.StateDir, "firecracker.sock")
	told := socket
	env := os.Environ()
	if jail != "" {
		if err := os.MkdirAll(jail, 0o700); err != nil {
			t.Fatal(err)
		}
		r.UID, socket, told = 0x70000000, h.api(spec.ID), "/api.sock"
		env = append(env, fakeJailEnv+"="+jail, fakeStateEnv+"="+spec.StateDir)
	}
	blob, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec.StateDir, "vm.json"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	vmm := exec.Command(binary, "--api-sock", told)
	vmm.Env = env
	vmm.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := vmm.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		vmm.Wait()
		close(exited)
	}()
	// Best effort: a pass has ended it already.
	t.Cleanup(func() { vmm.Process.Kill() })

	deadline := time.Now().Add(stopGrace)
	for !unloaded(socket) {
		if time.Now().After(deadline) {
			t.Fatalf("the vmm on %s did not answer within %s", socket, stopGrace)
		}
		time.Sleep(50 * time.Millisecond)
	}

	return exited
}

// requireUnloadedEnded proves a reopened provider ends an unloaded leftover within the daemon's probe budget, and a remove then frees its files.
func requireUnloadedEnded(t *testing.T, p models.Provider, spec models.SandboxSpec, exited <-chan struct{}, jail string) {
	t.Helper()

	// An attach waiting on a guest that never comes runs past this budget.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	status, err := p.Status(ctx, spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status of the unloaded leftover = %+v, %v, want stopped", status, err)
	}
	select {
	case <-exited:
	case <-time.After(stopGrace):
		t.Fatal("the unloaded vmm still runs after the new daemon read it")
	}
	if err := p.Remove(t.Context(), spec.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	gone := []string{filepath.Join(spec.StateDir, "vm.json"), filepath.Join(spec.StateDir, "firecracker.sock")}
	if jail != "" {
		gone = append(gone, filepath.Dir(jail))
	}
	for _, path := range gone {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after Remove: %v, want gone", path, err)
		}
	}
}

// unloaded says a vmm answers on the socket with nothing booted or loaded in it.
func unloaded(socket string) bool {
	_, info, err := fcapi.Adopt(context.Background(), socket, "")

	return err == nil && info.State == fcapi.StateNotStarted
}

// forkSpec is what the orchestrator hands Fork: an id, a directory and the bounds, and no entrypoint.
func (h *harness) forkSpec(t *testing.T) models.SandboxSpec {
	t.Helper()

	spec := h.newSpec(t)

	return models.SandboxSpec{ID: spec.ID, StateDir: spec.StateDir, Resources: spec.Resources}
}

// watchControls has the fake vmm note what the guest of s reads from here on.
func watchControls(t *testing.T, s models.SandboxSpec) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(s.StateDir, controlsFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// controls is what the guest of the sandbox under dir read, in order, of the kinds named.
func controls(t *testing.T, dir string, kinds ...string) []string {
	t.Helper()

	read, err := os.ReadFile(filepath.Join(dir, controlsFile))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for kind := range strings.FieldsSeq(string(read)) {
		if slices.Contains(kinds, kind) {
			got = append(got, kind)
		}
	}

	return got
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

// A remove leaves the state directory with nothing of the VM in it, and no jail; the directory itself is the repository's.
func TestRemoveDropsTheOverlayTheRecordAndTheJail(t *testing.T) {
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
	for _, path := range []string{filepath.Join(spec.StateDir, "overlay.raw"), filepath.Join(spec.StateDir, "vm.json"), filepath.Dir(h.jail(spec.ID))} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after Remove: %v, want gone", path, err)
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
		if _, err := h.reopen(t).Status(t.Context(), spec.ID); err != nil {
			t.Fatal(err)
		}
		logged = awaitLog(t, h.provider, spec.ID, logged)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(status.PID, syscall.SIGUSR2); err != nil {
		t.Fatalf("drop the fake vmm's streams: %v", err)
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
func awaitLog(t *testing.T, p *firecracker.Provider, id string, seen int) int {
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

// A cut pause leaves dir+".tmp" that resume never reads, so AdoptStaging drops it at daemon start (SHARD-404).
func TestAdoptStagingDropsACutPauseStage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snapshot")
	tmp := dir + ".tmp"
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatalf("stage a cut pause: %v", err)
	}

	if err := (&firecracker.Provider{}).AdoptStaging(dir); err != nil {
		t.Fatalf("AdoptStaging: %v", err)
	}

	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging %s survived adopt, want it dropped (err %v)", tmp, err)
	}
}
