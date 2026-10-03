// Package hostmem reads how much memory this host has.
package hostmem

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// parseMemTotal reads the MemTotal line of /proc/meminfo, which the kernel gives in KiB.
func parseMemTotal(r io.Reader) (int64, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("read MemTotal: %w", err)
		}

		return kib << 10, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}

	return 0, errors.New("/proc/meminfo has no MemTotal")
}
