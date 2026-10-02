//go:build linux

package hostmem

import (
	"fmt"
	"os"
)

// Total is the host's memory in bytes, as the kernel reports it.
func Total() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("read the host memory: %w", err)
	}
	defer f.Close()

	return parseMemTotal(f)
}
