package main

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// guestBoot is what -transport moves PID 1 onto before anything runs; the zero value boots nothing and serves from where it is.
type guestBoot struct {
	// Root is one ext4 disk that is the whole root.
	Root string
	// Base is a read-only EROFS image and Overlay the ext4 disk its overlayfs upper layer sits on; a microVM boots from the pair.
	Base, Overlay string
	// Console is the device the supervisor's stderr goes to once the root is in place; a guest without it keeps stderr where it was.
	Console string
	// Reboot ends the VM with a reboot instead of a power off, for a vmm that stays up after a power off.
	Reboot bool
	// SwapMiB is the swap file made on the disk the root writes to at each boot, 0 for none.
	SwapMiB int64
}

// set reports whether there is a root to move onto at all.
func (b guestBoot) set() bool { return b.Root != "" || b.Base != "" }

// check refuses a root that is both one disk and two, and a pair with one half missing.
func (b guestBoot) check() error {
	if b.Root != "" && (b.Base != "" || b.Overlay != "") {
		return errors.New("-root is one disk that is the whole root, so no -base or -overlay with it")
	}
	if (b.Base == "") != (b.Overlay == "") {
		return errors.New("-base and -overlay go together: the read-only image and the disk written over it")
	}
	if b.Reboot && !b.set() {
		return errors.New("-reboot ends a VM, so it needs the disk the VM boots from")
	}
	if b.SwapMiB < 0 {
		return fmt.Errorf("-swap is in MiB and cannot be negative, got %d", b.SwapMiB)
	}
	if b.SwapMiB > 0 && !b.set() {
		return errors.New("-swap makes its file on the disk the VM boots from, so it needs -root or -base and -overlay")
	}

	return nil
}

// swapHeader is the first page of a swap file as mkswap writes it, with the version 1 layout of union swap_header.
func swapHeader(size int64, pageSize int) ([]byte, error) {
	pages := size / int64(pageSize)
	// The header takes the first page, and last_page is 32 bits wide.
	if pages < 2 || pages-1 > int64(^uint32(0)) {
		return nil, fmt.Errorf("a swap file of %d bytes is not 2 to 2^32 pages of %d bytes", size, pageSize)
	}
	header := make([]byte, pageSize)
	binary.NativeEndian.PutUint32(header[1024:], 1)
	binary.NativeEndian.PutUint32(header[1028:], uint32(pages-1)) //nolint:gosec // bounded to 32 bits above
	copy(header[pageSize-10:], "SWAPSPACE2")

	return header, nil
}
