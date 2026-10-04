//go:build linux

package firecracker_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// inSessions is every process in one of sids that a SIGKILL can still end; a zombie holds nothing, and its parent reaps it.
func inSessions(sids map[int]bool) ([]int, error) {
	stats, err := filepath.Glob("/proc/[0-9]*/stat")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, stat := range stats {
		pid, err := strconv.Atoi(filepath.Base(filepath.Dir(stat)))
		if err != nil {
			return nil, fmt.Errorf("parse the pid of %s: %w", stat, err)
		}
		blob, err := os.ReadFile(stat)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// The fields after the name are state, ppid, pgrp and session.
		fields := strings.Fields(string(blob[bytes.LastIndexByte(blob, ')')+1:]))
		if len(fields) < 4 {
			return nil, fmt.Errorf("parse %s: %q", stat, blob)
		}
		sid, err := strconv.Atoi(fields[3])
		if err != nil {
			return nil, fmt.Errorf("parse the session in %s: %w", stat, err)
		}
		if sids[sid] && fields[0] != "Z" {
			pids = append(pids, pid)
		}
	}

	return pids, nil
}
