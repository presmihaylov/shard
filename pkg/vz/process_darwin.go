//go:build darwin

package vz

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// zombie is SZOMB in sys/proc.h, which x/sys does not name.
const zombie = 5

// startOf is the start time of pid in microseconds, from the kernel's process table.
func startOf(pid int) (int64, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return 0, fmt.Errorf("read the process table: %w", err)
	}
	if len(procs) == 0 || procs[0].Proc.P_stat == zombie {
		return 0, syscall.ESRCH
	}

	return startedAt(&procs[0]), nil
}

func startedAt(proc *unix.KinfoProc) int64 {
	started := proc.Proc.P_starttime

	return started.Sec*1e6 + int64(started.Usec)
}

// scan is the first live process whose arguments match; one whose arguments this user may not read is no shim of this daemon.
func scan(match func([]string) bool) (Process, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return Process{}, fmt.Errorf("read the process table: %w", err)
	}
	for i := range procs {
		pid := int(procs[i].Proc.P_pid)
		if pid <= 1 || procs[i].Proc.P_stat == zombie {
			continue
		}
		args, err := argsOf(pid)
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return Process{}, fmt.Errorf("read the arguments of pid %d: %w", pid, err)
		}
		if match(args) {
			return Process{PID: pid, Start: startedAt(&procs[i])}, nil
		}
	}

	return Process{}, nil
}

// argsOf reads kern.procargs2: argc, the executable path and its padding, then argc strings.
func argsOf(pid int) ([]string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	if len(buf) < 4 {
		return nil, fmt.Errorf("kern.procargs2 of pid %d holds %d bytes", pid, len(buf))
	}
	argc := int(binary.NativeEndian.Uint32(buf))
	rest := buf[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return nil, fmt.Errorf("kern.procargs2 of pid %d: no end to the executable path", pid)
	}
	rest = bytes.TrimLeft(rest[end:], "\x00")
	args := make([]string, 0, argc)
	for range argc {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return nil, fmt.Errorf("kern.procargs2 of pid %d: %d of %d arguments", pid, len(args), argc)
		}
		args = append(args, string(rest[:end]))
		rest = rest[end+1:]
	}

	return args, nil
}
