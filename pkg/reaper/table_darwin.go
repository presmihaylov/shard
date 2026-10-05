package reaper

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// zombie is SZOMB and exiting P_WEXIT in sys/proc.h, which x/sys does not name.
const (
	zombie  = 5
	exiting = 0x2000
)

// table is every process a SIGKILL can still end, with its session and start time; the kinfo of a process holds no session id, so getsid asks for each.
func table() ([]proc, error) {
	kinfos, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("list the processes: %w", err)
	}
	var procs []proc
	for _, k := range kinfos {
		pid := int(k.Proc.P_pid)
		// getsid(0) answers for the caller, not for the kernel.
		if pid == 0 || k.Proc.P_stat == zombie || k.Proc.P_flag&exiting != 0 {
			continue
		}
		sid, err := syscall.Getsid(pid)
		if errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("getsid %d: %w", pid, err)
		}
		procs = append(procs, proc{pid: pid, sid: sid, start: startOf(k)})
	}

	return procs, nil
}

func started(pid int) (string, error) {
	kinfos, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if len(kinfos) != 1 {
		return "", syscall.ESRCH
	}

	return startOf(kinfos[0]), nil
}

func startOf(k unix.KinfoProc) string {
	return fmt.Sprintf("%d.%06d", k.Proc.P_starttime.Sec, k.Proc.P_starttime.Usec)
}
