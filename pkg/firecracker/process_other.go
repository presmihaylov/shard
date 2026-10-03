//go:build !linux

package firecracker

import (
	"errors"
	"fmt"
	"syscall"
)

// process holds the vmm by its pid alone off Linux, where only the fake of the tests runs.
type process struct {
	pid int
}

func watch(pid int) (*process, error) {
	return &process{pid: pid}, nil
}

func (p *process) exited() (bool, error) {
	err := syscall.Kill(p.pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false, fmt.Errorf("probe firecracker %d: %w", p.pid, err)
	}

	return false, nil
}

func (p *process) release() error {
	return nil
}
