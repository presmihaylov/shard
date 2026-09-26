package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"

	"github.com/presmihaylov/shard/services/datadir"
	"github.com/presmihaylov/shard/services/provider/firecracker"
	"github.com/presmihaylov/shard/services/provider/gvisor"
	"github.com/presmihaylov/shard/services/provider/vzvm"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// KVMDevice is what a host exposes when it can run a virtual machine, and so what firecracker needs.
const KVMDevice = "/dev/kvm"

// Selection is the substrate a daemon runs sandboxes on, and why that one.
type Selection struct {
	Provider string
	Reason   string
}

// String is the line shard info prints: the substrate, then the reason it is the substrate.
func (s Selection) String() string {
	return s.Provider + ": " + s.Reason
}

// SelectProvider names the substrate a daemon started over this root with this --provider runs sandboxes
// on. A probe hands a host neither sysbox nor runc, since one is single-tenant and the other a plain
// container on the host kernel: only --provider or the root's own records name either.
func SelectProvider(named, root string) (Selection, error) {
	return selectProvider(named, root, KVMDevice)
}

func selectProvider(named, root, kvm string) (Selection, error) {
	if named != "" {
		return Selection{Provider: named, Reason: "named by --provider"}, nil
	}

	// A root that already holds records keeps the substrate that made them: no other one can read them.
	recorded, err := sandboxstate.RecordedProvider(root)
	if err != nil {
		return Selection{}, fmt.Errorf("read what made the records under %s: %w", root, err)
	}
	if recorded != "" {
		return Selection{Provider: recorded, Reason: "it made the records under " + root}, nil
	}

	// A firecracker root keeps its records inside its data image, which hides them whenever it is not mounted.
	image := datadir.ImagePath(root)
	_, err = os.Stat(image)
	if err == nil {
		return Selection{Provider: firecracker.Name, Reason: "it made the data image " + image}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Selection{}, fmt.Errorf("stat %s: %w", image, err)
	}
	// A Mac has no /dev/kvm and runs its virtual machines through the framework, so the probe below says nothing there.
	if runtime.GOOS == "darwin" {
		return Selection{Provider: vzvm.Name, Reason: "macOS runs virtual machines through Virtualization.framework"}, nil
	}

	// A node this process cannot open runs no microVM, so the probe opens it rather than stat it.
	dev, err := os.OpenFile(kvm, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return Selection{Provider: gvisor.Name, Reason: "no " + kvm}, nil
	}
	if err != nil {
		return Selection{Provider: gvisor.Name, Reason: fmt.Sprintf("%s does not open: %v", kvm, err)}, nil
	}
	if err := dev.Close(); err != nil {
		return Selection{}, fmt.Errorf("close %s: %w", kvm, err)
	}

	return Selection{Provider: firecracker.Name, Reason: kvm + " opens"}, nil
}
