//go:build darwin

package firecracker

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// zombie is SZOMB and inExit is P_WEXIT in sys/proc.h, which x/sys does not name.
const (
	zombie = 5
	inExit = 0x2000
)

// exiting says no live process holds pid: xnu can link a dial that races the close of an exiting listener to a socket nobody ever ends (SHARD-754).
func exiting(pid int) (bool, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return false, fmt.Errorf("read the state of pid %d: %w", pid, err)
	}

	return len(procs) == 0 || procs[0].Proc.P_stat == zombie || procs[0].Proc.P_flag&inExit != 0, nil
}
