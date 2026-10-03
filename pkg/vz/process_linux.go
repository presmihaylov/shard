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

// scan is the first live process whose arguments match; one gone before its arguments were read is no shim.
func scan(match func([]string) bool) (Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return Process{}, fmt.Errorf("list the processes: %w", err)
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		// A name that is no number is not a process.
		if err != nil || pid <= 1 {
			continue
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return Process{}, fmt.Errorf("read the arguments of pid %d: %w", pid, err)
		}
		if !match(strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")) {
			continue
		}
		shim, err := Identify(pid)
		if errors.Is(err, syscall.ESRCH) {
			continue
		}

		return shim, err
	}

	return Process{}, nil
}
