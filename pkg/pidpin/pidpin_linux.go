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

// gone reads the pidfd, which turns readable once its process exits.
func gone(h handle, _ int) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(h), Events: unix.POLLIN}} //nolint:gosec // G115: the kernel hands out an fd that fits an int32
	n, err := unix.Poll(fds, 0)
	if err != nil {
		return false, err
	}

	return n > 0 && fds[0].Revents&unix.POLLIN != 0, nil
}

func release(h handle) error {
	return unix.Close(int(h))
}
