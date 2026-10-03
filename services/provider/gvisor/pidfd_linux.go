//go:build linux

package gvisor

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// pidfdKill pins pid before still runs, so the check and the SIGKILL reach one process even if the pid is reused.
func pidfdKill(pid int, still func() (bool, error)) (err error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return fmt.Errorf("pin process %d: %w", pid, err)
	}
	defer func() {
		if cerr := unix.Close(fd); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close the pidfd of process %d: %w", pid, cerr))
		}
	}()

	// A pid reused before the pin fails still; the pinned process reaped after still fails the signal with ESRCH.
	ok, err := still()
	if err != nil || !ok {
		return err
	}

	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil {
		return fmt.Errorf("kill process %d: %w", pid, err)
	}

	return nil
}
