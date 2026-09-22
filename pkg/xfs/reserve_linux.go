//go:build linux

package xfs

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// reserve creates the image file and allocates its blocks up front, so a full disk fails here and not under a sandbox.
func reserve(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := unix.Fallocate(int(f.Fd()), 0, 0, size); err != nil {
		return errors.Join(fmt.Errorf("fallocate %d bytes at %s: %w", size, path, err), f.Close(), os.Remove(path))
	}

	return f.Close()
}
