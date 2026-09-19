package bundle

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// clonefile shares the blocks of src with dst, which APFS does in one call; another filesystem says so.
func clonefile(src, dst string) error {
	err := unix.Clonefile(src, dst, 0)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) {
		return fmt.Errorf("%w: %w", err, errors.ErrUnsupported)
	}

	return err
}
