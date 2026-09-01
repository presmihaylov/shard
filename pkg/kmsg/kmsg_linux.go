//go:build linux

package kmsg

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// recordSize is the largest record the kernel hands out; a smaller read fails with EINVAL.
const recordSize = 8192

// Read is every record the ring holds now, oldest first; it needs root.
func Read() ([]Line, error) {
	now := time.Now()
	mark := fmt.Sprintf("%s%d-%d", markPrefix, os.Getpid(), now.UnixNano())
	if err := os.WriteFile("/dev/kmsg", []byte("<7>"+mark+"\n"), 0); err != nil {
		return nil, fmt.Errorf("write the kernel log: %w", err)
	}

	records, err := drain()
	if err != nil {
		return nil, err
	}
	return lines(records, mark, now)
}

// drain reads the ring from its oldest record until the kernel has no more.
func drain() ([]record, error) {
	// Raw syscalls, not os.File: Go would put a non-blocking, pollable fd in its poller and wait for the next record instead of returning EAGAIN.
	fd, err := syscall.Open("/dev/kmsg", syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open the kernel log: %w", err)
	}
	defer syscall.Close(fd)

	var records []record
	buf := make([]byte, recordSize)
	for {
		n, err := syscall.Read(fd, buf)
		if errors.Is(err, syscall.EAGAIN) {
			return records, nil
		}
		// The ring overran the reader: the next read starts at the oldest record left.
		if errors.Is(err, syscall.EPIPE) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the kernel log: %w", err)
		}
		if n == 0 {
			return records, nil
		}

		r, err := parse(string(buf[:n]))
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
}
