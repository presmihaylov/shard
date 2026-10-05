//go:build linux || darwin

package pty

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func pollableMaster(pair *Pty) (*Pty, error) {
	// A Mac master can join Go's poller only after its replica exists.
	fd, err := unix.FcntlInt(pair.Master.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("duplicate the terminal master: %w", err), pair.Close())
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, errors.Join(fmt.Errorf("make the terminal master nonblocking: %w", err), unix.Close(fd), pair.Close())
	}

	master := os.NewFile(uintptr(fd), pair.Master.Name())
	if err := master.SetWriteDeadline(time.Time{}); err != nil {
		return nil, errors.Join(fmt.Errorf("enable terminal write deadlines: %w", err), master.Close(), pair.Close())
	}
	if err := pair.Master.Close(); err != nil {
		return nil, errors.Join(fmt.Errorf("close the original terminal master: %w", err), master.Close(), pair.Replica.Close())
	}
	pair.Master = master

	return pair, nil
}
