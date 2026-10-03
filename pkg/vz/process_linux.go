//go:build linux

package vz

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// startOf is the start time of pid in clock ticks since boot, field 22 of its stat.
func startOf(pid int) (int64, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, syscall.ESRCH
	}
	if err != nil {
		return 0, err
	}
	// The command name may hold spaces and parens, so the fields start after the last one.
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return 0, fmt.Errorf("parse /proc/%d/stat: no command name", pid)
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return 0, fmt.Errorf("parse /proc/%d/stat: %d fields after the name", pid, len(fields))
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return 0, syscall.ESRCH
	}
	start, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse the start time of pid %d: %w", pid, err)
	}

	return start, nil
}
