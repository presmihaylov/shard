//go:build linux

package pidpin

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// handle is a pidfd, which names its process until the handle closes, past the process's exit and reap.
type handle int

func open(pid int) (handle, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return -1, err
	}

	return handle(fd), nil
}

func signal(h handle, sig syscall.Signal) error {
	return unix.PidfdSendSignal(int(h), sig, nil, 0)
}

func release(h handle) error {
	return unix.Close(int(h))
}
