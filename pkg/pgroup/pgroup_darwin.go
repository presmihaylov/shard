package pgroup

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// zombie is SZOMB and exiting P_WEXIT in sys/proc.h, which x/sys does not name.
const (
	zombie  = 5
	exiting = 0x2000
)

// noneLive says every member of the group is a zombie or past the point of exit, which darwin no longer signals.
func noneLive(pgid int) (bool, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return false, fmt.Errorf("list the processes of group %d: %w", pgid, err)
	}
	for _, p := range procs {
		if p.Proc.P_stat != zombie && p.Proc.P_flag&exiting == 0 {
			return false, nil
		}
	}

	return true, nil
}
