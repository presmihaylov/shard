//go:build darwin

package vz

import (
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
	started := procs[0].Proc.P_starttime

	return started.Sec*1e6 + int64(started.Usec), nil
}
