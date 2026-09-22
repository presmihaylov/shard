package store

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// exchange swaps a and b by RENAME_SWAP, which APFS does in one call; another filesystem says so.
func exchange(a, b string) error {
	err := unix.RenameatxNp(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_SWAP)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("%w: %w", err, errors.ErrUnsupported)
	}

	return err
}
