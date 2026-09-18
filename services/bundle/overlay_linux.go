//go:build linux

package bundle

import (
	"fmt"
	"slices"
	"strings"
	"syscall"

	"github.com/presmihaylov/shard/pkg/mountinfo"
)

// Mount stacks the sandbox's writable layer over the shared image rootfs. It is safe to call twice.
func (b Bundle) Mount(lower string) error {
	// The upper layer lives on the disk, so the disk is up before the overlay stacks on it.
	if err := b.MountDisk(); err != nil {
		return err
	}

	mounted, err := b.Mounted()
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}

	options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, b.Upper, b.Work)
	if err := syscall.Mount("overlay", b.RootFS, "overlay", 0, options); err != nil {
		return fmt.Errorf("mount the overlay on %s: %w", b.RootFS, err)
	}

	return nil
}

// Unmount drops the merged view, then the disk under it; the layers stay in the image, which a stop and start relies on.
func (b Bundle) Unmount() error {
	mounted, err := b.Mounted()
	if err != nil {
		return err
	}

	if mounted {
		// MNT_DETACH, because a leftover open file in the guest must not make a stop fail.
		if err := syscall.Unmount(b.RootFS, syscall.MNT_DETACH); err != nil {
			return fmt.Errorf("unmount %s: %w", b.RootFS, err)
		}
	}

	return b.UnmountDisk()
}

// Mounted asks the kernel rather than a record, because a shard restart forgets what it mounted.
func (b Bundle) Mounted() (bool, error) {
	m, found, err := mountinfo.At(b.RootFS)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	// Refuse rather than run on a mount we did not make, or detach one we do not own.
	if m.FSType != "overlay" || !slices.Contains(strings.Split(m.SuperOptions, ","), "upperdir="+b.Upper) {
		return false, fmt.Errorf("%s already holds a %s mount that is not this sandbox's overlay", b.RootFS, m.FSType)
	}

	return true, nil
}
