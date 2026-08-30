//go:build linux

package kmsg

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// recordSize is the largest record the kernel hands out; a smaller read fails with EINVAL.
const recordSize = 8192

// Read is every record the ring holds now, oldest first; it needs root.
func Read() ([]Line, error) {
	boot, err := bootTime()
	if err != nil {
		return nil, err
	}

	// Raw syscalls, not os.File: Go would put a non-blocking, pollable fd in its poller and wait for the next record instead of returning EAGAIN.
	fd, err := syscall.Open("/dev/kmsg", syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open the kernel log: %w", err)
	}
	defer syscall.Close(fd)

	var lines []Line
	buf := make([]byte, recordSize)
	for {
		n, err := syscall.Read(fd, buf)
		if errors.Is(err, syscall.EAGAIN) {
			return lines, nil
		}
		// The ring overran the reader: the next read starts at the oldest record left.
		if errors.Is(err, syscall.EPIPE) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the kernel log: %w", err)
		}
		if n == 0 {
			return lines, nil
		}

		line, err := parse(string(buf[:n]), boot)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
}

// bootTime is the wall time the kernel clock started, read from /proc/stat btime.
func bootTime() (time.Time, error) {
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, fmt.Errorf("read /proc/stat: %w", err)
	}

	for line := range strings.Lines(string(stat)) {
		value, ok := strings.CutPrefix(line, "btime ")
		if !ok {
			continue
		}

		secs, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse btime in /proc/stat: %w", err)
		}

		return time.Unix(secs, 0), nil
	}

	return time.Time{}, errors.New("/proc/stat has no btime")
}
