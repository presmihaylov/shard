//go:build !linux

package bundle

import (
	"errors"
	"io/fs"
	"os"

	"github.com/presmihaylov/shard/models"
)

// errNoDisk keeps a developer Mac honest the way errNoOverlay does: a sandbox disk is a loop mount.
var errNoDisk = errors.New("the sandbox disk needs a loop mount, which only Linux has")

func (b Bundle) Provision(models.Resources) error { return errNoDisk }

func (b Bundle) MountDisk() error { return errNoDisk }

func (b Bundle) UnmountDisk() error { return errNoDisk }

// KeepDisk has nothing to mount for a bundle never provisioned, the only kind here.
func (b Bundle) KeepDisk() error {
	if _, err := os.Stat(b.Image); errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	return errNoDisk
}

// withDisk runs fn over the host layers: no disk can exist here, so there is nothing to mount around it.
func (b Bundle) withDisk(fn func() error) error {
	defer b.lockDisk()()

	return fn()
}
