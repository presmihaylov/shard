package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"

	"github.com/presmihaylov/shard/services/provider/firecracker"
	"github.com/presmihaylov/shard/services/provider/gvisor"
	"github.com/presmihaylov/shard/services/provider/vzvm"
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

// SelectProvider names the substrate a daemon started with this --provider runs sandboxes on. Only
// --provider names sysbox or runc: one is single-tenant and the other is a plain container, so a host
// is never handed either of them by a probe.
func SelectProvider(named string) Selection {
	return selectProvider(named, KVMDevice)
}

func selectProvider(named, kvm string) Selection {
	if named != "" {
		return Selection{Provider: named, Reason: "named by --provider"}
	}
	// A Mac has no /dev/kvm and runs its virtual machines through the framework, so the probe below says nothing there.
	if runtime.GOOS == "darwin" {
		return Selection{Provider: vzvm.Name, Reason: "macOS runs virtual machines through Virtualization.framework"}
	}

	_, err := os.Stat(kvm)
	if err == nil {
		return Selection{Provider: firecracker.Name, Reason: kvm + " is present"}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return Selection{Provider: gvisor.Name, Reason: "no " + kvm}
	}

	return Selection{Provider: gvisor.Name, Reason: fmt.Sprintf("%s does not answer: %v", kvm, err)}
}
