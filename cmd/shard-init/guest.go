package main

import "errors"

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

	return nil
}
