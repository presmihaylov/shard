package vz

import (
	"encoding/json"
	"errors"
	"fmt"
	"syscall"
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

// Locate is the live shim started on the config for socket, as one an older daemon booted has no record of its pid; zero is none (SHARD-423).
func Locate(socket string) (Process, error) {
	return scan(func(args []string) bool { return serves(args, socket) })
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

// Kill ends the process while its pid still names it, and nothing once it does not.
func (p Process) Kill() error {
	alive, err := p.Alive()
	if err != nil || !alive {
		return err
	}

	return killPID(p.PID)
}

// killPID ends a shim, and the group it leads with it; one that leads none dies alone.
func killPID(pid int) error {
	pgid, err := syscall.Getpgid(pid)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the group of the shim %d: %w", pid, err)
	}
	if err := syscall.Kill(target(pid, pgid, syscall.Getpgrp()), syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill the shim %d: %w", pid, err)
	}

	return nil
}

// target is the group when the shim leads it and the daemon is not in it, and the pid alone otherwise.
func target(pid, pgid, own int) int {
	if pgid == pid && pgid != own {
		return -pid
	}

	return pid
}
