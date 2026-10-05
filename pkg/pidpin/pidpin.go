// Package pidpin holds a process by a handle the kernel binds to it, so a signal sent through the handle never reaches a later process that took its pid.
package pidpin

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
)

// Process is one process held since Open; its pid alone names whoever holds the number now.
type Process struct {
	pid int
	// mu orders Kill against Close, since a pidfd number closed here may name another process's pidfd next.
	mu       sync.Mutex
	handle   handle
	released bool
}

// Open holds the process on pid now; an error wrapping syscall.ESRCH says none holds it that a signal can reach.
func Open(pid int) (*Process, error) {
	// 0 and 1 are no process to end: the caller's own group, or every process it may signal.
	if pid <= 1 {
		return nil, fmt.Errorf("pin pid %d: no process to signal", pid)
	}
	h, err := open(pid)
	if err != nil {
		return nil, fmt.Errorf("pin pid %d: %w", pid, err)
	}

	return &Process{pid: pid, handle: h}, nil
}

// PID is the number the process held at Open.
func (p *Process) PID() int { return p.pid }

// Kill sends SIGKILL to the held process alone; one that has exited since is no error.
func (p *Process) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released {
		return fmt.Errorf("kill pid %d: its pin is released", p.pid)
	}
	err := signal(p.handle, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("kill pid %d: %w", p.pid, err)
	}

	return nil
}

// Exited says the held process has exited, a zombie included, so whatever took its pid since never reads as it.
func (p *Process) Exited() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released {
		return false, fmt.Errorf("probe pid %d: its pin is released", p.pid)
	}
	done, err := gone(p.handle, p.pid)
	if err != nil {
		return false, fmt.Errorf("probe pid %d: %w", p.pid, err)
	}

	return done, nil
}

// Close lets the handle go; a second Close does nothing.
func (p *Process) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released {
		return nil
	}
	p.released = true
	if err := release(p.handle); err != nil {
		return fmt.Errorf("release pid %d: %w", p.pid, err)
	}

	return nil
}
