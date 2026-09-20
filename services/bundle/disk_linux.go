//go:build linux

package bundle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/diskimage"
)

// Provision makes the disk if none exists, sized to the bound, and mounts it, so Build, Fork and Clone write the layers into it.
func (b Bundle) Provision(r models.Resources) error {
	if err := os.MkdirAll(b.Disk, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", b.Disk, err)
	}

	if err := diskimage.Create(b.Image, DiskBytes(r)); err != nil {
		return err
	}

	return b.MountDisk()
}

// MountDisk mounts the disk alone, which a clone needs of its stopped source to copy the layers off.
func (b Bundle) MountDisk() error {
	if _, err := os.Stat(b.Image); err != nil {
		return fmt.Errorf("the sandbox has no disk: %w", err)
	}

	return diskimage.Mount(b.Image, b.Disk)
}

// UnmountDisk detaches the disk. The overlay stacks on it, so the overlay goes first.
func (b Bundle) UnmountDisk() error {
	return diskimage.Unmount(b.Image, b.Disk)
}

// withDisk runs fn over a mounted disk and leaves it as found; a bundle never provisioned, as a unit test's, keeps its layers on the host.
func (b Bundle) withDisk(fn func() error) error {
	defer b.lockDisk()()

	if _, err := os.Stat(b.Image); errors.Is(err, fs.ErrNotExist) {
		return fn()
	}

	mounted, err := diskimage.Mounted(b.Image, b.Disk)
	if err != nil {
		return err
	}
	if mounted {
		return fn()
	}

	if err := b.MountDisk(); err != nil {
		return err
	}

	return errors.Join(fn(), b.UnmountDisk())
}
