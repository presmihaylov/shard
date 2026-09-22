// Package firecracker runs sandboxes as Firecracker microVMs on Linux with /dev/kvm, one firecracker process per sandbox.
package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/supervisor"
)

// Name is the substrate, as the record and every refusal name it.
const Name = "firecracker"

// Binary is the vmm the provider drives, on PATH wherever firecracker is installed.
const Binary = "firecracker"

// cmdline boots the guest onto the serial console and hands shard-init the vsock transport, its two disks in attach order, and -reboot: firecracker exits on a guest reboot, never on a power off.
const cmdline = "console=ttyS0 reboot=k panic=1 pci=off -- -transport vsock -base /dev/vda -overlay /dev/vdb -console /dev/ttyS0 -reboot"

// MinMemoryMiB is the smallest --memory a guest boots with: the kernel and shard-init keep 32 MiB, and the bound needs room under that.
const MinMemoryMiB = 128

// MaxVCPUs is the most firecracker gives one guest.
const MaxVCPUs = 32

// The files under a sandbox's state directory, all the provider's own; the overlay disk is bundle.OverlayDiskFile.
const (
	recordFile   = "vm.json"
	socketFile   = "firecracker.sock"
	vsockFile    = "vsock.sock"
	consoleFile  = "console.log"
	exitFile     = "exit.json"
	restartsFile = "restarts.json"
	// oomFile marks a guest the memory bound ended; the cgroup a Linux provider reads instead is gone with the VM.
	oomFile    = "oom"
	logFile    = "output.log"
	initrdFile = "initrd.cpio"
	// memoryFile is the guest memory a restore mapped, a hard link to the snapshot's own; a fresh boot has none.
	memoryFile = "memory"
)

// The files under a snapshot directory, beside a copy of the overlay; the marker goes in last.
const (
	snapshotState = "vmstate"
	snapshotFile  = "snapshot.json"
	// checkpointFile is what the sandbox service takes as a complete snapshot after a restart of the daemon.
	checkpointFile = "checkpoint.img"
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
	// startGrace bounds the wait for the supervisor to answer on vsock once the vmm is up.
	startGrace = 30 * time.Second
)

// StateDirs answers where a sandbox's directory is. sandboxstate.Repository.Dir is what shard passes.
type StateDirs func(id string) (string, error)

// Config is what the provider boots every microVM with.
type Config struct {
	// Binary is the firecracker to spawn, and Kernel the guest kernel it boots.
	Binary string
	Kernel string
	// Init is a static linux shard-init for the host's arch, which becomes the initrd's /init.
	Init string
	// Dir is where the provider writes the initrd it builds from Init.
	Dir  string
	Dirs StateDirs
}

var _ models.Provider = (*Provider)(nil)

// Provider implements models.Provider on Firecracker.
type Provider struct {
	cfg    Config
	initrd string
	// cgroupRoot is the host cgroup v2 mount, under which every vmm is bounded. A test points it at a directory it owns, or at nothing.
	cgroupRoot string

	mu sync.Mutex
	// machines is every vmm this daemon has spoken to; one it has not is adopted by its socket.
	machines map[string]*machine
}

func New(cfg Config) (*Provider, error) {
	if cfg.Binary == "" || cfg.Kernel == "" || cfg.Init == "" || cfg.Dir == "" || cfg.Dirs == nil {
		return nil, errors.New("the firecracker provider needs a binary, a kernel, a shard-init, a directory and a state directory lookup")
	}

	// The directory is the provider's own, so a fresh data root gets it here and not from every caller.
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("create the firecracker directory: %w", err)
	}
	initrd := filepath.Join(cfg.Dir, initrdFile)
	if err := bundle.WriteInitrd(cfg.Init, initrd); err != nil {
		return nil, err
	}

	return &Provider{cfg: cfg, initrd: initrd, cgroupRoot: cgroup.Root, machines: map[string]*machine{}}, nil
}

func (p *Provider) Name() string { return Name }

// Capabilities are the three snapshot verbs, which every host with /dev/kvm has: a snapshot is two files the vmm writes.
func (p *Provider) Capabilities() models.Capabilities {
	return models.Capabilities{Pause: true, Resume: true, Fork: true}
}

// CheckResources is checkResources before any record exists, so a refused --memory leaves no failed sandbox in ls.
func (p *Provider) CheckResources(res models.Resources) error { return checkResources(res) }

// Close drops what this process holds of every vmm and leaves the VMs running, which is what a daemon exit does.
func (p *Provider) Close() error {
	p.mu.Lock()
	held := p.machines
	p.machines = map[string]*machine{}
	p.mu.Unlock()

	var errs []error
	for _, m := range held {
		if err := m.close(); err != nil {
			errs = append(errs, fmt.Errorf("sandbox %s: %w", m.id, err))
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
	// Tap is the host end the vmm opens, the bridge port the host rules name; empty boots the VM without a network.
	Tap string `json:"tap,omitempty"`
	// Address is the guest's prefix and Gateway the bridge's address, which the guest is told over the control stream.
	Address string `json:"address,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	// Nameservers and Hostname are the resolver files the guest writes itself, as a VM has no upper layer.
	Nameservers []string `json:"nameservers,omitempty"`
	Hostname    string   `json:"hostname,omitempty"`
	// RootFS is the image tree an exec resolves a named user against.
	RootFS    string             `json:"rootfs,omitempty"`
	Resources models.Resources   `json:"resources"`
	Run       supervisor.RunSpec `json:"run"`
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
