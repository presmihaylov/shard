//go:build linux

package firecracker

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// process holds the vmm by a pidfd: the vmm is no child of this process, and the pidfd still says when it exits.
type process struct {
	pid int
	fd  int
}

// watch takes a vmm that is already gone as exited, so the caller reports it with its console.
func watch(pid int) (*process, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return &process{pid: pid, fd: -1}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("watch firecracker %d: %w", pid, err)
	}

	return &process{pid: pid, fd: fd}, nil
}

// exited is whether the vmm is gone, without a wait: a pidfd polls readable once its process exits.
func (p *process) exited() (bool, error) {
	if p.fd < 0 {
		return true, nil
	}
	fds := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}} //nolint:gosec // a kernel fd is a C int
	n, err := unix.Poll(fds, 0)
	if errors.Is(err, unix.EINTR) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("poll firecracker %d: %w", p.pid, err)
	}

	return n > 0, nil
}

func (p *process) release() error {
	if p.fd < 0 {
		return nil
	}
	if err := unix.Close(p.fd); err != nil {
		return fmt.Errorf("close the pidfd of firecracker %d: %w", p.pid, err)
	}

	return nil
}
