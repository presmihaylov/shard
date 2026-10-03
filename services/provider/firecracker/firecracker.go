// Package firecracker runs sandboxes as Firecracker microVMs on Linux with /dev/kvm, one firecracker process per sandbox.
package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
	fcapi "github.com/presmihaylov/shard/pkg/firecracker"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/supervisor"
)

// Name is the substrate, as the record and every refusal name it.
const Name = "firecracker"

// Binary is the vmm the provider drives, and Jailer what spawns it, both on PATH wherever firecracker is installed.
const (
	Binary = "firecracker"
	Jailer = "jailer"
)

// cmdline boots the guest onto the serial console and hands shard-init the vsock transport, its two disks in attach order, and -reboot: firecracker exits on a guest reboot, never on a power off.
const cmdline = "console=ttyS0 reboot=k panic=1 pci=off -- -transport vsock -base /dev/vda -overlay /dev/vdb -console /dev/ttyS0 -reboot"

// MinMemoryMiB is the smallest --memory a guest boots with: the kernel and shard-init keep 32 MiB, and the bound needs room under that.
const MinMemoryMiB = 128

// MaxVCPUs is the most firecracker gives one guest.
const MaxVCPUs = 32

// The files under a sandbox's state directory, all the provider's own; the overlay disk is bundle.OverlayDiskFile.
const (
	recordFile = "vm.json"
	// socketFile and vsockFile are where a vmm a daemon before the jail spawned still answers (SHARD-306).
	socketFile   = "firecracker.sock"
	vsockFile    = "vsock.sock"
	consoleFile  = "console.log"
	exitFile     = "exit.json"
	restartsFile = "restarts.json"
	// oomFile marks a guest the memory bound ended; the cgroup a Linux provider reads instead is gone with the VM.
	oomFile = "oom"
	// supervisorFailedFile holds the reason shard-init gave for its own death, which the halt would otherwise take with the guest.
	supervisorFailedFile = "supervisor-failed"
	logFile              = "output.log"
	// memoryFile is the guest memory in a snapshot, and a link to it in a state directory from before the jail.
	memoryFile = "memory"
	// cursorFile places the guest's output in the log, so an attach after a daemon restart resumes it; a fresh boot drops it.
	cursorFile = "output.cursor"
	// restoringFile marks a fork's restore in flight, whose guest holds the source's address until the readdress (SHARD-321).
	restoringFile = "restoring"
	// reseedFile marks a restored guest still on the snapshot's crng key, so a daemon that adopts it reseeds it first (SHARD-266).
	reseedFile = "reseed"
)

// The files under a snapshot directory, beside a copy of the overlay; the marker goes in last.
const (
	snapshotState = "vmstate"
	snapshotFile  = "snapshot.json"
	// checkpointFile is what the sandbox service takes as a complete snapshot after a restart of the daemon.
	checkpointFile = "checkpoint.img"
	// snapshotFileMode is the one place the snapshot files get their mode, once they are root's again.
	snapshotFileMode os.FileMode = 0o600
)

// The files under Config.Dir, the provider's own.
const (
	initrdFile = "initrd.cpio"
	// execFile is the copy of Binary the jailer runs, under a name of its own, so the jail path does not follow a symlink's target.
	execFile   = "firecracker"
	kernelFile = "vmlinux"
	uidFile    = "next-uid"
)

// The files in a jail, as the vmm names them; the jailer makes the jail its "/".
const (
	jailKernel  = "/vmlinux"
	jailInitrd  = "/initrd"
	jailBase    = "/base.erofs"
	jailOverlay = "/" + bundle.OverlayDiskFile
	jailState   = "/" + snapshotState
	jailMemory  = "/" + memoryFile
	apiSocket   = "/api.sock"
	jailVsock   = "/v.sock"
	// jailSnap is where a pause has the vmm write its snapshot, apart from the memory a restored vmm maps.
	jailSnap = "/snap"
)

// Every vmm runs as a uid of its own, gid the same, in a range clear of Sysbox, the /etc/subuid defaults and the systemd ranges.
const (
	firstUID = 0x70000000
	lastUID  = 0x7FFDFFFF
)

// The drive ids on the API, in the order the guest sees them as /dev/vda and /dev/vdb.
const (
	baseDrive    = "base"
	overlayDrive = "overlay"
)

const (
	// pollInterval paces every wait here; a vmm state read is one socket round trip.
	pollInterval = 100 * time.Millisecond
	// killGrace bounds the wait after a forced stop of the VM, which nothing in the guest can refuse.
	killGrace = 10 * time.Second
	// flushGrace bounds the best-effort flush a forced stop asks of the guest; a slower or hung guest is cut with the VM.
	flushGrace = 5 * time.Second
	// probeFloor is the least one vmm state read gets, so a wait whose time ran out still asks once (SHARD-388).
	probeFloor = time.Second
	// adoptBound is how long a vmm met only by its socket gets to answer before it reads unresponsive; a boot probes in parallel, so it serves inside 5 s (SHARD-392).
	adoptBound = 4 * time.Second
	// startGrace bounds the wait for the supervisor to answer on vsock once the vmm is up.
	startGrace = 30 * time.Second
)

// JailSockets names every socket a sandbox's vmm binds in its jail under base, so the daemon refuses a root they do not fit under.
func JailSockets(base, id string) []string {
	root := jailRoot(base, id)

	return []string{filepath.Join(root, apiSocket), filepath.Join(root, jailVsock)}
}

// jailRoot is the chroot the jailer makes for the sandbox: the base, the exec file's name, the id, then root.
func jailRoot(base, id string) string {
	return filepath.Join(base, execFile, id, "root")
}

// StateDirs answers where a sandbox's directory is. sandboxstate.Repository.Dir is what shard passes.
type StateDirs func(id string) (string, error)

// Config is what the provider boots every microVM with.
type Config struct {
	// Binary is the firecracker to run, Jailer the jailer that runs it, and Kernel the guest kernel it boots.
	Binary string
	Jailer string
	Kernel string
	// Init is a static linux shard-init for the host's arch, which becomes the initrd's /init.
	Init string
	// Dir is where the provider keeps its copies of Binary and Kernel, and the initrd it builds from Init.
	Dir string
	// JailBase is the jailer's chroot base, on the reflink filesystem of Dir and the state directories, so each jail gets its files by reference.
	JailBase string
	Dirs     StateDirs
	// Snapshots answers where a sandbox's pause writes, which an adopt checks before it resumes a paused VM. sandboxstate.Repository.SnapshotDir is what shard passes.
	Snapshots StateDirs
	// Log takes what an operator must see of a guest, such as a refused control line; nil discards it.
	Log *log.Logger
}

var _ models.Provider = (*Provider)(nil)

// Provider implements models.Provider on Firecracker.
type Provider struct {
	cfg    Config
	exec   string
	kernel string
	initrd string
	// cgroupRoot is the host cgroup v2 mount, under which every vmm is bounded. A test points it at a directory it owns, or at nothing.
	cgroupRoot string

	mu sync.Mutex
	// machines is every vmm this daemon has spoken to; one it has not is adopted by its socket.
	machines map[string]*machine
	// spawning is every sandbox this process is bringing a vmm up for, which no lookup may take for a leftover.
	spawning map[string]bool
	// unadopted is every vmm an adopt found silent, held unattached so each lookup waits on its one request and never dials anew.
	unadopted map[string]*machine
	// adopting closes when the one adopt in flight for a sandbox ends, so a racing lookup reuses what it made.
	adopting map[string]chan struct{}

	// uids orders the uid counter, apart from mu, so a spawn's file write holds up no status.
	uids sync.Mutex
	// chown and ownTap give a jail's files and the tap to the vmm's uid; a test without root swaps them.
	chown  func(path string, uid, gid int) error
	ownTap func(namespace, name string, uid, gid int) error
	// lostRuns keeps the loss of a forgotten machine, so every later verb still answers with it until rm (SHARD-290).
	lostRuns map[string]error
}

func New(cfg Config) (*Provider, error) {
	if cfg.Binary == "" || cfg.Jailer == "" || cfg.Kernel == "" || cfg.Init == "" || cfg.Dir == "" || cfg.JailBase == "" || cfg.Dirs == nil || cfg.Snapshots == nil {
		return nil, errors.New("the firecracker provider needs a binary, a jailer, a kernel, a shard-init, a directory, a jail base, a state directory lookup and a snapshot directory lookup")
	}
	// Every pause takes a Diff, which only firecracker 1.13 and newer take without a dirty-page log (SHARD-450).
	if err := fcapi.CheckVersion(cfg.Binary); err != nil {
		return nil, err
	}

	// The directory is the provider's own, so a fresh data root gets it here and not from every caller.
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("create the firecracker directory: %w", err)
	}
	if err := os.MkdirAll(cfg.JailBase, 0o700); err != nil {
		return nil, fmt.Errorf("create the jail base: %w", err)
	}
	initrd := filepath.Join(cfg.Dir, initrdFile)
	if err := bundle.WriteInitrd(cfg.Init, initrd); err != nil {
		return nil, err
	}
	// The jailer copies the exec file into each jail with its mode, and the vmm runs it as a uid that owns nothing else.
	exec := filepath.Join(cfg.Dir, execFile)
	if err := copyIn(cfg.Binary, exec, 0o755); err != nil {
		return nil, err
	}
	kernel := filepath.Join(cfg.Dir, kernelFile)
	if err := copyIn(cfg.Kernel, kernel, 0o600); err != nil {
		return nil, err
	}
	if cfg.Log == nil {
		cfg.Log = log.New(io.Discard, "", 0)
	}

	return &Provider{
		cfg: cfg, exec: exec, kernel: kernel, initrd: initrd, cgroupRoot: cgroup.Root,
		machines: map[string]*machine{}, spawning: map[string]bool{}, lostRuns: map[string]error{},
		unadopted: map[string]*machine{}, adopting: map[string]chan struct{}{},
		chown: os.Chown, ownTap: netns.ChownTapIn,
	}, nil
}

// copyIn puts a copy of src at dst, which a jail can then take by reference; an unchanged copy is not written again.
func copyIn(src, dst string, perm os.FileMode) error {
	blob, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if err := store.WriteFileIfChanged(dst, blob, perm); err != nil {
		return fmt.Errorf("copy %s in: %w", src, err)
	}

	return nil
}

func (p *Provider) Name() string { return Name }

// Capabilities are the three snapshot verbs, which every host with /dev/kvm has: a snapshot is two files the vmm writes.
func (p *Provider) Capabilities() models.Capabilities {
	return models.Capabilities{Pause: true, Resume: true, Fork: true}
}

// CheckResources is checkResources before any record exists, so a refused --memory leaves no failed sandbox in ls.
func (p *Provider) CheckResources(res models.Resources) error { return checkResources(res) }

// AdmitDisk reserves the overlay a create writes into dir, before the sandbox has a record, so a refusal leaves none.
func (p *Provider) AdmitDisk(dir string, res models.Resources) error {
	return bundle.Reserve(filepath.Join(dir, bundle.OverlayDiskFile), bundle.DiskBytes(res))
}

// ReleaseDisk gives back what AdmitDisk reserved for a create that ended before its record.
func (p *Provider) ReleaseDisk(dir string) { bundle.Release(dir) }

// Close drops what this process holds of every vmm and leaves the VMs running; only tests call it, because the daemon relies on its own process exit.
func (p *Provider) Close() error {
	p.mu.Lock()
	held := p.machines
	unadopted := p.unadopted
	p.machines = map[string]*machine{}
	p.unadopted = map[string]*machine{}
	p.mu.Unlock()

	var errs []error
	for _, set := range []map[string]*machine{held, unadopted} {
		for _, m := range set {
			if err := m.close(); err != nil {
				errs = append(errs, fmt.Errorf("sandbox %s: %w", m.id, err))
			}
		}
	}

	return errors.Join(errs...)
}

// ReleaseRoot has nothing to give back: a VM pins nothing under the root between sandboxes.
func (p *Provider) ReleaseRoot() error { return nil }

// record is vm.json: what a boot and a restart of the daemon need to know about the sandbox.
type record struct {
	// BaseDisk is the image's EROFS file, which every boot attaches read-only under the overlay.
	BaseDisk string `json:"base_disk"`
	// Tap is the tap the vmm opens in its namespace, named like the host port the rules key on; empty boots the VM without a network.
	Tap string `json:"tap,omitempty"`
	// Address is the guest's prefix and Gateway the bridge's address, which the guest is told over the control stream.
	Address string `json:"address,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	// Nameservers and Hostname are the resolver files the guest writes itself, as a VM has no upper layer.
	Nameservers []string `json:"nameservers,omitempty"`
	Hostname    string   `json:"hostname,omitempty"`
	// RootFS is the image tree a start reads the CA roots from; an exec resolves a named user in the guest (SHARD-356).
	RootFS    string             `json:"rootfs,omitempty"`
	Resources models.Resources   `json:"resources"`
	Run       supervisor.RunSpec `json:"run"`
	// UID is the uid and gid the vmm runs as, which a start and a resume keep; zero is a record from before the jail (SHARD-306).
	UID int `json:"uid,omitempty"`
	// Jail is the chroot the vmm runs in; empty is a vmm a daemon before the jail spawned, which answers in the state directory.
	Jail string `json:"jail,omitempty"`
}

// sockets is where the sandbox's vmm answers: in its jail, or in the state directory for one spawned before the jail.
func (r record) sockets(dir string) (api, vsock string) {
	if r.Jail == "" {
		return filepath.Join(dir, socketFile), filepath.Join(dir, vsockFile)
	}

	return filepath.Join(r.Jail, apiSocket), filepath.Join(r.Jail, jailVsock)
}

// nextUID hands out the uid of a new vmm; none is used twice, so nothing a gone sandbox left is open to a new one.
func (p *Provider) nextUID() (int, error) {
	p.uids.Lock()
	defer p.uids.Unlock()
	path := filepath.Join(p.cfg.Dir, uidFile)
	next, err := readUID(path)
	if err != nil {
		return 0, err
	}
	if next < firstUID || next > lastUID {
		return 0, fmt.Errorf("the next uid in %s is %d, outside %d..%d, so no sandbox can get a uid of its own", path, next, firstUID, lastUID)
	}
	if err := store.WriteFile(path, []byte(strconv.Itoa(next+1)), 0o600); err != nil {
		return 0, fmt.Errorf("write the next uid: %w", err)
	}

	return next, nil
}

func readUID(path string) (int, error) {
	blob, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return firstUID, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the next uid: %w", err)
	}
	uid, err := strconv.Atoi(strings.TrimSpace(string(blob)))
	if err != nil {
		return 0, fmt.Errorf("parse the next uid in %s: %w", path, err)
	}

	return uid, nil
}

func (p *Provider) dir(id string) (string, error) {
	dir, err := p.cfg.Dirs(id)
	if err != nil {
		return "", err
	}

	return dir, nil
}

// readRecord reads vm.json; found is false for an id the provider never held, or one a remove forgot.
func readRecord(dir string) (record, bool, error) {
	blob, err := os.ReadFile(filepath.Join(dir, recordFile))
	if errors.Is(err, fs.ErrNotExist) {
		return record{}, false, nil
	}
	if err != nil {
		return record{}, false, fmt.Errorf("read the vm record: %w", err)
	}

	var r record
	if err := json.Unmarshal(blob, &r); err != nil {
		return record{}, false, fmt.Errorf("decode the vm record in %s: %w", dir, err)
	}

	return r, true, nil
}

func writeRecord(dir string, r record) error {
	return writeJSON(filepath.Join(dir, recordFile), r)
}

func writeJSON(path string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	if err := store.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// vcpus is the guest's count: the request, or every host CPU firecracker can give when the request is zero.
func vcpus(requested int) int64 {
	if requested > 0 {
		return int64(requested)
	}

	return int64(min(max(runtime.NumCPU(), 1), MaxVCPUs))
}

// LogPath names the file the entrypoint's output lands in, pumped off the guest's logs port.
func (p *Provider) LogPath(id string) (string, error) {
	dir, err := p.dir(id)
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, logFile), nil
}

// HeldLogs is the console log: the vmm holds it, while the daemon itself writes the output log and rotates it as it writes.
func (p *Provider) HeldLogs(id string) ([]string, error) {
	dir, err := p.dir(id)
	if err != nil {
		return nil, err
	}

	return []string{filepath.Join(dir, consoleFile)}, nil
}

// BoundOutputLog bounds an output log a daemon before the bound left past max; the caller runs it before any attach, while no FileLog writes the log.
func (p *Provider) BoundOutputLog(id string, max int64) error {
	dir, err := p.dir(id)
	if err != nil {
		return err
	}

	return supervisor.BoundLog(filepath.Join(dir, logFile), filepath.Join(dir, cursorFile), max)
}
