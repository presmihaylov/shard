package store

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// exchange swaps a and b by RENAME_EXCHANGE, which XFS, Btrfs and ext4 do in one call; another filesystem says so.
func exchange(a, b string) error {
	err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
		return fmt.Errorf("%w: %w", err, errors.ErrUnsupported)
	}

	return err
}
