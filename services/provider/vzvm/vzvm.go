// Package vzvm runs sandboxes as Virtualization.framework VMs on macOS, one shard-vz-shim per sandbox.
package vzvm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cpio"
	"github.com/presmihaylov/shard/pkg/netstack"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/services/supervisor"
)

// Name is the substrate, as the record and every refusal name it.
const Name = "vz"

// cmdline boots the guest onto the console and hands shard-init the vsock transport and the root disk.
const cmdline = "console=hvc0 -- -transport vsock -root /dev/vda"

// The files under a sandbox's state directory, all the provider's own.
const (
	recordFile   = "vm.json"
	diskFile     = "disk.img"
	socketFile   = "shim.sock"
	consoleFile  = "console.log"
	exitFile     = "exit.json"
	restartsFile = "restarts.json"
	logFile      = "output.log"
	initrdFile   = "initrd.cpio"
)

// The files a snapshot directory holds: the saved VM, its disk at the save, and what a restore must know.
const (
	snapshotFile     = "snapshot.json"
	snapshotState    = "vm.vzvmstate"
	snapshotDiskFile = "disk.img"
	// checkpointFile is the completion marker the sandbox service reads, the name gVisor's own checkpoint has.
	checkpointFile = "checkpoint.img"
)

const (
	// pollInterval paces every wait here; a shim state read is one socket round trip.
	pollInterval = 100 * time.Millisecond
	// killGrace bounds the wait after a forced stop of the VM, which nothing in the guest can refuse.
	killGrace = 10 * time.Second
	// startGrace bounds the wait for the supervisor to answer on vsock once the shim is up.
	startGrace = 30 * time.Second
)

// StateDirs answers where a sandbox's directory is. sandboxstate.Repository.Dir is what shard passes.
type StateDirs func(id string) (string, error)

// Config is what the provider boots every VM with.
type Config struct {
	// Shim is the shard-vz-shim binary, and Kernel the guest kernel it boots.
	Shim   string
	Kernel string
	// Init is a static linux/arm64 shard-init, which becomes the initrd's /init.
	Init string
	// Dir is where the provider writes the initrd it builds from Init.
	Dir string
	// Stack terminates every VM's frames; nil boots each VM without a network device.
	Stack *netstack.Stack
	Dirs  StateDirs
	// SaveRestore says the framework on this host saves and restores a VM, which is what a fork needs; vz.HostSaveRestore probes it.
	SaveRestore bool
}

var _ models.Provider = (*Provider)(nil)

// Provider implements models.Provider on Virtualization.framework.
type Provider struct {
	cfg    Config
	initrd string

	mu sync.Mutex
	// machines is every shim this daemon has spoken to; a shim it has not is adopted by its socket.
	machines map[string]*machine
}

func New(cfg Config) (*Provider, error) {
	if cfg.Shim == "" || cfg.Kernel == "" || cfg.Init == "" || cfg.Dir == "" || cfg.Dirs == nil {
		return nil, errors.New("the vz provider needs a shim, a kernel, a shard-init, a directory and a state directory lookup")
	}

	initrd, err := buildInitrd(cfg.Init, filepath.Join(cfg.Dir, initrdFile))
	if err != nil {
		return nil, err
	}

	return &Provider{cfg: cfg, initrd: initrd, machines: map[string]*machine{}}, nil
}

// buildInitrd packs shard-init as /init of a newc archive, so the kernel runs it as PID 1 with nothing else in the initramfs.
func buildInitrd(initPath, path string) (string, error) {
	body, err := os.ReadFile(initPath)
	if err != nil {
		return "", fmt.Errorf("read shard-init: %w", err)
	}

	var archive bytes.Buffer
	w := cpio.New(&archive)
	if err := w.File("init", 0o755, body); err != nil {
		return "", fmt.Errorf("pack shard-init into the initrd: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("finish the initrd: %w", err)
	}
	if err := store.WriteFile(path, archive.Bytes(), 0o600); err != nil {
		return "", fmt.Errorf("write the initrd: %w", err)
	}

	return path, nil
}

func (p *Provider) Name() string { return Name }

// Capabilities: the three optional verbs are one VZ save and two restores, so a host without them has none.
func (p *Provider) Capabilities() models.Capabilities {
	return models.Capabilities{Pause: p.cfg.SaveRestore, Resume: p.cfg.SaveRestore, Fork: p.cfg.SaveRestore}
}

// ReleaseRoot has nothing to give back: a VM pins nothing under the root between sandboxes.
func (p *Provider) ReleaseRoot() error { return nil }

// record is vm.json: what a boot, a restart of the daemon and a snapshot need to know about the sandbox.
type record struct {
	// MachineID is the identifier the shim made on the first boot, which every later boot must reuse.
	MachineID string `json:"machine_id,omitempty"`
	// Address is the guest's prefix and Gateway the stack's address; empty boots the VM without a network.
	Address string `json:"address,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	// RootFS is the image tree an exec resolves a named user against.
	RootFS    string             `json:"rootfs,omitempty"`
	Resources models.Resources   `json:"resources"`
	Run       supervisor.RunSpec `json:"run"`
	// Paused says the last verb was a pause: the VM is saved into the snapshot and its shim is gone.
	Paused bool `json:"paused,omitempty"`
}

// snapshot is snapshot.json: what a restore of the saved state beside it must reuse.
type snapshot struct {
	MachineID string             `json:"machine_id"`
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

// LogPath names the file the entrypoint's output lands in, pumped off the guest's logs port.
func (p *Provider) LogPath(id string) (string, error) {
	dir, err := p.dir(id)
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, logFile), nil
}
