//go:build !linux

package firecracker_test

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// inSessions is every process in one of sids that a SIGKILL can still end; Darwin's ps prints no session id, so getsid asks for each.
func inSessions(sids map[int]bool) ([]int, error) {
	out, err := exec.Command("ps", "-A", "-o", "pid=,stat=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	var pids []int
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("parse the ps line %q", line)
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("parse the ps line %q: %w", line, err)
		}
		if strings.HasPrefix(fields[1], "Z") {
			continue
		}
		sid, err := syscall.Getsid(pid)
		if errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("getsid %d: %w", pid, err)
		}
		if sids[sid] {
			pids = append(pids, pid)
		}
	}

	return pids, nil
}
