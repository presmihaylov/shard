package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/presmihaylov/shard/services/datadir"
	"github.com/presmihaylov/shard/services/provider/firecracker"
	"github.com/presmihaylov/shard/services/provider/gvisor"
	"github.com/presmihaylov/shard/services/provider/runc"
	"github.com/presmihaylov/shard/services/provider/sysbox"
	"github.com/presmihaylov/shard/services/provider/vzvm"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// KVMDevice is what a host exposes when it can run a virtual machine, and so what firecracker needs.
const KVMDevice = "/dev/kvm"

// Providers names every substrate a daemon can run sandboxes on, in the order the help lists them.
var Providers = []string{gvisor.Name, sysbox.Name, runc.Name, vzvm.Name, firecracker.Name}

// unknownProvider refuses a name no substrate answers to, and names every one that does.
func unknownProvider(name string) error {
	last := len(Providers) - 1

	return fmt.Errorf("unknown provider %q: shard knows %s and %s", name, strings.Join(Providers[:last], ", "), Providers[last])
}

// Selection is the substrate a daemon runs sandboxes on, and why that one.
type Selection struct {
	Provider string
	Reason   string
	// Unreadable names the records that would not read while the substrate was chosen, and is empty when all read.
	Unreadable string
}

// String is the line shard info prints: the substrate, then the reason it is the substrate.
func (s Selection) String() string {
	return s.Provider + ": " + s.Reason
}

// withNote carries the unreadable-records note onto a selection a later branch returns.
func (s Selection) withNote(note string) Selection {
	s.Unreadable = note

	return s
}

// SelectProvider names the substrate a daemon started over this root with this --provider runs sandboxes
// on. A probe hands a host neither sysbox nor runc, since one is single-tenant and the other a plain
// container on the host kernel: only --provider or the root's own records name either.
func SelectProvider(named, root string) (Selection, error) {
	return selectProvider(named, root, KVMDevice)
}

func selectProvider(named, root, kvm string) (Selection, error) {
	// Before the root is read, so a typo fails before any daemon work.
	if named != "" && !slices.Contains(Providers, named) {
		return Selection{}, unknownProvider(named)
	}

	made, err := madeBy(root)
	if err != nil {
		return Selection{}, err
	}
	if named != "" && made.Provider != "" && named != made.Provider {
		return Selection{}, fmt.Errorf("--provider %s contradicts the root, which is %s's: %s; name %s or leave --provider out", named, made.Provider, made.Reason, made.Provider)
	}
	if named != "" {
		return Selection{Provider: named, Reason: "named by --provider"}.withNote(made.Unreadable), nil
	}
	if made.Provider != "" {
		return made, nil
	}

	// A root whose every record is unreadable still belongs to one substrate, so a probe must not relabel it; only --provider recovers it (SHARD-343).
	if made.Unreadable != "" {
		return Selection{}, fmt.Errorf("the root %s holds records that cannot be read, so its provider is unknown; name it with shard daemon --provider: %s", root, made.Unreadable)
	}

	// A Mac has no /dev/kvm and runs its virtual machines through the framework, so the probe below says nothing there.
	if runtime.GOOS == "darwin" {
		return Selection{Provider: vzvm.Name, Reason: "macOS runs virtual machines through Virtualization.framework"}.withNote(made.Unreadable), nil
	}

	// A node this process cannot open runs no microVM, so the probe opens it rather than stat it.
	dev, err := os.OpenFile(kvm, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return Selection{Provider: gvisor.Name, Reason: "no " + kvm}.withNote(made.Unreadable), nil
	}
	if err != nil {
		return Selection{Provider: gvisor.Name, Reason: fmt.Sprintf("%s does not open: %v", kvm, err)}.withNote(made.Unreadable), nil
	}
	if err := dev.Close(); err != nil {
		return Selection{}, fmt.Errorf("close %s: %w", kvm, err)
	}

	return Selection{Provider: firecracker.Name, Reason: kvm + " opens"}.withNote(made.Unreadable), nil
}

// madeBy names the substrate that made what is under root, or none for a fresh root. No other one can read it.
func madeBy(root string) (Selection, error) {
	recorded, err := sandboxstate.RecordedProvider(root)

	// An unreadable record must not stop a daemon from choosing a substrate; a fatal read still does (SHARD-343).
	var unreadable *sandboxstate.UnreadableError
	if err != nil && !errors.As(err, &unreadable) {
		return Selection{}, fmt.Errorf("read what made the records under %s: %w", root, err)
	}

	var note string
	if unreadable != nil {
		note = fmt.Sprintf("some records cannot be read: %v", err)
	}

	if recorded != "" {
		return Selection{Provider: recorded, Reason: "it made the records under " + root, Unreadable: note}, nil
	}

	// A firecracker root keeps its records inside its data image, which hides them whenever it is not mounted.
	image := datadir.ImagePath(root)
	_, err = os.Stat(image)
	if err == nil {
		return Selection{Provider: firecracker.Name, Reason: "it made the data image " + image, Unreadable: note}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Selection{}, fmt.Errorf("stat %s: %w", image, err)
	}

	return Selection{Unreadable: note}, nil
}
