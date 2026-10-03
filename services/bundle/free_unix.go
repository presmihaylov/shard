//go:build darwin || linux

package bundle

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// freeBytes is the space an unprivileged writer can still take on the filesystem that holds dir.
func freeBytes(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}

	return int64(st.Bavail) * int64(st.Bsize), nil //nolint:gosec // G115: no disk holds 8 EiB free
}
