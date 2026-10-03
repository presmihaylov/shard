//go:build linux

package vz

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

// scan is the first live process of the daemon's user, run from shim, whose arguments match; argv alone is anyone's to write.
func scan(shim string, match func([]string) bool) (Process, error) {
	// The kernel names the executable by its resolved path.
	want, err := filepath.EvalSymlinks(shim)
	if err != nil {
		return Process{}, fmt.Errorf("resolve the shim %s: %w", shim, err)
	}
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
		uid, err := ownerOf(pid)
		if gone(err) {
			continue
		}
		if err != nil {
			return Process{}, fmt.Errorf("read the owner of pid %d: %w", pid, err)
		}
		if uid != os.Geteuid() {
			continue
		}
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		// A kernel thread has no executable, and one that is not dumpable hides it from this user.
		if gone(err) || errors.Is(err, fs.ErrPermission) {
			continue
		}
		if err != nil {
			return Process{}, fmt.Errorf("read the executable of pid %d: %w", pid, err)
		}
		// An upgrade renames a new shim over the path, so the one an older daemon ran reads deleted.
		if strings.TrimSuffix(exe, " (deleted)") != want {
			continue
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if gone(err) {
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

// ownerOf is the effective uid of pid, from its status; the owner of /proc/<pid> reads root for a process that is not dumpable.
func ownerOf(pid int) (int, error) {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for line := range strings.Lines(string(status)) {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "Uid:" {
			continue
		}
		uid, err := strconv.Atoi(fields[2])
		if err != nil {
			return 0, fmt.Errorf("parse the uid of pid %d: %w", pid, err)
		}

		return uid, nil
	}

	return 0, fmt.Errorf("the status of pid %d names no uid", pid)
}

// gone says the process ended before its file was read.
func gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}
