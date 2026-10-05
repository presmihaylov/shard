package reaper

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

// table is every process a SIGKILL can still end, with its session and start time.
func table() ([]proc, error) {
	stats, err := filepath.Glob("/proc/[0-9]*/stat")
	if err != nil {
		return nil, err
	}
	var procs []proc
	for _, stat := range stats {
		pid, err := strconv.Atoi(filepath.Base(filepath.Dir(stat)))
		if err != nil {
			return nil, fmt.Errorf("parse the pid of %s: %w", stat, err)
		}
		fields, err := statFields(stat)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if fields[0] == "Z" {
			continue
		}
		sid, err := strconv.Atoi(fields[3])
		if err != nil {
			return nil, fmt.Errorf("parse the session in %s: %w", stat, err)
		}
		procs = append(procs, proc{pid: pid, sid: sid, start: fields[19]})
	}

	return procs, nil
}

func started(pid int) (string, error) {
	fields, err := statFields(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}

	return fields[19], nil
}

// statFields are the fields after the name: state, ppid, pgrp, session, and so on to starttime at 19.
func statFields(stat string) ([]string, error) {
	blob, err := os.ReadFile(stat)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(blob[bytes.LastIndexByte(blob, ')')+1:]))
	if len(fields) < 20 {
		return nil, fmt.Errorf("parse %s: %q", stat, blob)
	}

	return fields, nil
}
