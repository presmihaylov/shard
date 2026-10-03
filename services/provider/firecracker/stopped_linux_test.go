//go:build linux

package firecracker_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// stopped is whether every thread of pid is in the stopped state; a thread that exits between the list and the read is no longer one of them.
func stopped(pid int) (bool, error) {
	tasks, err := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/stat", pid))
	if err != nil {
		return false, err
	}
	if len(tasks) == 0 {
		return false, fmt.Errorf("process %d is gone", pid)
	}
	for _, task := range tasks {
		blob, err := os.ReadFile(task)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		fields := strings.Fields(string(blob[bytes.LastIndexByte(blob, ')')+1:]))
		if len(fields) == 0 || fields[0] != "T" {
			return false, nil
		}
	}

	return true, nil
}
