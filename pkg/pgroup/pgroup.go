// Package pgroup signals a process group, and answers a group left with no live process as gone on every host.
package pgroup

import (
	"errors"
	"syscall"
)

// Kill sends sig to every process in the group pgid; an error wrapping syscall.ESRCH says no live process is left in it.
func Kill(pgid int, sig syscall.Signal) error {
	err := syscall.Kill(-pgid, sig)
	if !errors.Is(err, syscall.EPERM) {
		return err
	}
	// Darwin answers EPERM, not ESRCH, for a group whose members are all exiting or exited and not yet reaped.
	gone, listErr := noneLive(pgid)
	if listErr != nil {
		return errors.Join(err, listErr)
	}
	if gone {
		return syscall.ESRCH
	}

	return err
}
