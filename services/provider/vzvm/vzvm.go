// Package vzvm runs sandboxes as Virtualization.framework VMs on macOS, one shard-vz-shim per sandbox.
package vzvm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netstack"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/pkg/vz"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/supervisor"
)

// Name is the substrate, as the record and every refusal name it.
const Name = "vz"

// cmdline boots the guest, hands shard-init the vsock transport and the root disk; the shim ends the VM when the kernel prints its panic banner on the console.
const cmdline = "console=hvc0 -- -transport vsock -root /dev/vda"

// bootCmdline adds the swap shard-init makes on the root disk at each boot.
func bootCmdline(res models.Resources) string {
	return fmt.Sprintf("%s -swap %d", cmdline, res.SwapMiB)
}

// MinMemoryMiB is the smallest --memory a guest boots with: the kernel and shard-init keep 32 MiB, and the bound needs room under that.
const MinMemoryMiB = 128

// DefaultMemoryMiB is the memory a create that names none gets, as a guest has no unbounded memory.
const DefaultMemoryMiB = 512

// The files under a sandbox's state directory, all the provider's own.
const (
	recordFile  = "vm.json"
	diskFile    = "disk.img"
	socketFile  = "shim.sock"
	consoleFile = "console.log"
	// oomFile marks a guest the memory bound ended; the cgroup a Linux provider reads instead is gone with the VM.
	oomFile    = "oom"
	initrdFile = "initrd.cpio"
	// shimFile names the shim the last attach verified, which a refused dial cannot (SHARD-423).
	shimFile = "shim.json"
	// supervisorFailedFile holds the reason shard-init gave for a death at boot, which the end of the shim would otherwise take with the guest.
	supervisorFailedFile = "supervisor-failed"
	// captureDir is where a fork stages the save of its source, in the fork's own state directory.
	captureDir = "capture"
)

// The files a checkpoint directory holds: the saved VM, its disk at the save, and what a restore must know.
const (
	checkpointMeta     = "checkpoint.json"
	checkpointState    = "vm.vzvmstate"
	checkpointDiskFile = "disk.img"
	// checkpointFile is the completion marker the sandbox service reads, the name gVisor's own checkpoint has.
	checkpointFile = "checkpoint.img"
)

const (
	// pollInterval paces every wait here; a shim state read is one socket round trip.
	pollInterval = 100 * time.Millisecond
	// killGrace bounds the wait after a forced stop of the VM, which nothing in the guest can refuse.
	killGrace = 10 * time.Second
	// flushGrace bounds the best-effort flush a forced stop asks of the guest; a slower or hung guest is cut with the VM.
	flushGrace = 5 * time.Second
	// probeFloor is the least one shim state read gets, so a wait whose time ran out still asks once (SHARD-349).
	probeFloor = time.Second
	// adoptBound is how long a shim met only by its socket gets to answer before it reads unresponsive (SHARD-422).
	adoptBound = 5 * time.Second
	// startGrace bounds the wait for the supervisor to answer on vsock once the shim is up.
	startGrace = 30 * time.Second
)

// redialGrace bounds the control stream a save's run dials again, past which the source is reported frozen; a test shortens it.
var redialGrace = startGrace

// SocketFiles names every socket the provider binds in a sandbox's state directory, so the daemon refuses a root they do not fit under.
func SocketFiles() []string { return []string{socketFile} }

// StateDirs answers where a sandbox's directory is. sandboxstate.Repository.Dir is what shard passes.
type StateDirs func(id string) (string, error)

// Config is what the provider boots every VM with.
type Config struct {
	// Shim is the shard-vz-shim binary, and Kernel the guest kernel it boots.
	Shim   string
	Kernel string
	// KernelTag names Kernel in the record of every sandbox this provider boots.
	KernelTag string
	// Init is a static linux/arm64 shard-init, which becomes the initrd's /init.
	Init string
	// Dir is where the provider writes the initrd it builds from Init.
	Dir string
	// Stack terminates every VM's frames; nil boots each VM without a network device.
	Stack *netstack.Stack
	Dirs  StateDirs
	// SaveRestore says the framework on this host saves and restores a VM, which is what a fork needs; vz.HostSaveRestore probes it.
	SaveRestore bool
	// Log takes what an operator must see of a guest, such as a refused control line; nil discards it.
	Log *log.Logger
}

var _ models.Provider = (*Provider)(nil)

// Provider implements models.Provider on Virtualization.framework.
type Provider struct {
	cfg    Config
	initrd string

	mu sync.Mutex
	// machines is every shim this daemon has spoken to; a shim it has not is adopted by its socket.
	machines map[string]*machine
	// unadopted is every shim an adopt found silent, held unattached so each lookup waits on its one request and never dials anew.
	unadopted map[string]*machine
	// adopting closes when the one adopt in flight for a sandbox ends, so a racing lookup reuses what it made.
	adopting map[string]chan struct{}
	// recovering is nil but in a test, which holds the gap between the choice to thaw a lost freeze and that thaw.
	recovering func()
}

func New(cfg Config) (*Provider, error) {
	if cfg.Shim == "" || cfg.Kernel == "" || cfg.Init == "" || cfg.Dir == "" || cfg.Dirs == nil {
		return nil, errors.New("the vz provider needs a shim, a kernel, a shard-init, a directory and a state directory lookup")
	}

	initrd := filepath.Join(cfg.Dir, initrdFile)
	if err := bundle.WriteInitrd(cfg.Init, initrd); err != nil {
		return nil, err
	}
	if cfg.Log == nil {
		cfg.Log = log.New(io.Discard, "", 0)
	}

	return &Provider{cfg: cfg, initrd: initrd, machines: map[string]*machine{}, unadopted: map[string]*machine{}, adopting: map[string]chan struct{}{}}, nil
}

func (p *Provider) Name() string { return Name }

// GuestKernel is the tag of the kernel a fresh boot runs; a restored VM runs the one its memory image holds.
func (p *Provider) GuestKernel() string { return p.cfg.KernelTag }

// DefaultMemoryMiB is the memory the sandbox service gives a create that names none.
func (p *Provider) DefaultMemoryMiB() int64 { return DefaultMemoryMiB }

// CheckResources is checkMemory before any record exists, so a refused --memory leaves no failed sandbox in ls.
func (p *Provider) CheckResources(res models.Resources) error { return checkResources(res) }

// AdmitDisk reserves the disk a create clones into dir, before the sandbox has a record, so a refusal leaves none.
func (p *Provider) AdmitDisk(dir string, res models.Resources) error {
	return bundle.Reserve(filepath.Join(dir, diskFile), bundle.DiskBytes(res))
}

// ReleaseDisk gives back what AdmitDisk reserved for a create that ended before its record.
func (p *Provider) ReleaseDisk(dir string) { bundle.Release(dir) }

// Close drops what this process holds of every shim and leaves the VMs running; only tests call it, because the daemon relies on its own process exit.
func (p *Provider) Close() error {
	p.mu.Lock()
	held := p.machines
	p.machines = map[string]*machine{}
	p.unadopted = map[string]*machine{}
	p.mu.Unlock()

	var errs []error
	for _, m := range held {
		if err := m.close(); err != nil {
			errs = append(errs, fmt.Errorf("sandbox %s: %w", m.id, err))
		}
	}

	return errors.Join(errs...)
}

// Capabilities: pause, resume and fork are each a VZ save or a restore, so a host without them has none; a port rides vsock on every host, and swap is the guest kernel's own.
func (p *Provider) Capabilities() models.Capabilities {
	return models.Capabilities{Pause: p.cfg.SaveRestore, Resume: p.cfg.SaveRestore, Fork: p.cfg.SaveRestore, Port: true, Swap: true}
}

// ReleaseRoot has nothing to give back: a VM pins nothing under the root between sandboxes.
func (p *Provider) ReleaseRoot() error { return nil }

// record is vm.json: what a boot, a restart of the daemon and a checkpoint need to know about the sandbox.
type record struct {
	// MachineID is the identifier the shim made on the first boot, which every later boot must reuse.
	MachineID string `json:"machine_id,omitempty"`
	// Address is the guest's prefix and Gateway the stack's address; empty boots the VM without a network.
	Address string `json:"address,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	// Nameservers and Hostname are the resolver files the guest writes itself, as a VM has no upper layer.
	Nameservers []string `json:"nameservers,omitempty"`
	Hostname    string   `json:"hostname,omitempty"`
	// RootFS is the image tree a start reads the CA roots from; an exec resolves a named user in the guest (SHARD-356).
	RootFS string `json:"rootfs,omitempty"`
	// Roots are the CA roots a seed's disk held at create, which a later trust reads in place of the image's (SHARD-784).
	Roots     *bundle.Roots    `json:"roots,omitempty"`
	Resources models.Resources `json:"resources"`
	Run       supervisor.Base  `json:"run"`
	// Paused says the last verb was a pause: the VM is saved into the checkpoint and its shim is gone.
	Paused bool `json:"paused,omitempty"`
	// Pauses counts them, so a checkpoint a crashed pause staged is told from the one it meant to replace.
	Pauses int `json:"pauses,omitempty"`
}

// checkpoint is checkpoint.json: what a restore of the saved state beside it must reuse.
type checkpoint struct {
	MachineID string           `json:"machine_id"`
	Pause     int              `json:"pause"`
	RootFS    string           `json:"rootfs,omitempty"`
	Roots     *bundle.Roots    `json:"roots,omitempty"`
	Resources models.Resources `json:"resources"`
	Run       supervisor.Base  `json:"run"`
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

// readShim is the shim the last attach recorded, or the live one started on the socket, as an older daemon recorded none; zero is neither.
func (p *Provider) readShim(dir string) (vz.Process, error) {
	blob, err := os.ReadFile(filepath.Join(dir, shimFile))
	if errors.Is(err, fs.ErrNotExist) {
		return vz.Locate(p.cfg.Shim, filepath.Join(dir, socketFile))
	}
	if err != nil {
		return vz.Process{}, fmt.Errorf("read the shim record: %w", err)
	}
	var shim vz.Process
	if err := json.Unmarshal(blob, &shim); err != nil {
		return vz.Process{}, fmt.Errorf("decode the shim record in %s: %w", dir, err)
	}

	return shim, nil
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

// HeldLogs is the console log: the vmm holds it, while the daemon itself writes each process log and rotates it as it writes.
func (p *Provider) HeldLogs(id string) ([]string, error) {
	dir, err := p.dir(id)
	if err != nil {
		return nil, err
	}

	return []string{filepath.Join(dir, consoleFile)}, nil
}

// BoundOutputLog bounds each process log a daemon before the bound left past max; the caller runs it before any attach, while no FileLog writes them.
func (p *Provider) BoundOutputLog(id string, max int64) error {
	dir, err := p.dir(id)
	if err != nil {
		return err
	}

	return supervisor.BoundProcessLogs(dir, max)
}
