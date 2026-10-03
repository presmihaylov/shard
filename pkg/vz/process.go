package vz

import (
	"encoding/json"
	"errors"
	"fmt"
	"syscall"

	"github.com/presmihaylov/shard/pkg/pidpin"
)

// Process is a shim by its pid and its start time, so a pid the kernel gave to another process later is never signalled.
type Process struct {
	PID   int   `json:"pid"`
	Start int64 `json:"start"`
}

// Identify reads the start time of a live pid; a pid that is gone, or a zombie, is ESRCH.
func Identify(pid int) (Process, error) {
	if pid <= 1 {
		return Process{}, fmt.Errorf("identify pid %d: not a shim", pid)
	}
	start, err := startOf(pid)
	if err != nil {
		return Process{}, fmt.Errorf("identify pid %d: %w", pid, err)
	}

	return Process{PID: pid, Start: start}, nil
}

// Locate is the live shim the binary at shim started on the config for socket, as one an older daemon booted has no record of its pid; zero is none (SHARD-423).
func Locate(shim, socket string) (Process, error) {
	return scan(shim, func(args []string) bool { return serves(args, socket) })
}

// serves says the arguments are the ones Start gives a shim for socket.
func serves(args []string, socket string) bool {
	if len(args) != 3 || args[1] != "-config" {
		return false
	}
	var cfg Config
	if err := json.Unmarshal([]byte(args[2]), &cfg); err != nil {
		return false
	}

	return cfg.Socket == socket
}

// Alive says the pid still names this process; a zero Process names none.
func (p Process) Alive() (bool, error) {
	if p.PID <= 1 {
		return false, nil
	}
	start, err := startOf(p.PID)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read pid %d: %w", p.PID, err)
	}

	return start == p.Start, nil
}

// Kill ends the process while its pid still names it, and nothing once it does not; the check and the signal reach one pinned process.
func (p Process) Kill() error {
	return p.kill(func(*Process) {})
}

// kill pins the pid, checks the start time and signals through the pin; between runs after the check, where a test moves the pid to another process.
func (p Process) kill(between func(*Process)) error {
	if p.PID <= 1 {
		return nil
	}
	pin, err := pidpin.Open(p.PID)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("kill the shim %d: %w", p.PID, err)
	}
	// A pid names one process from its fork to its reap, so a start time that still matches after the pin proves the pin holds this process.
	alive, err := p.Alive()
	if err != nil || !alive {
		return errors.Join(err, pin.Close())
	}
	between(&p)
	if err := pin.Kill(); err != nil {
		return errors.Join(fmt.Errorf("kill the shim %d: %w", pin.PID(), err), pin.Close())
	}

	return pin.Close()
}
