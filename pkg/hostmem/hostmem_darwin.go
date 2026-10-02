//go:build darwin

package hostmem

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

// Total is the host's memory in bytes, as the kernel reports it.
func Total() (int64, error) {
	size, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("read the host memory: sysctl hw.memsize: %w", err)
	}
	if size > math.MaxInt64 {
		return 0, fmt.Errorf("read the host memory: sysctl hw.memsize is %d bytes", size)
	}

	return int64(size), nil
}
