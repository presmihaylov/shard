package bundle

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// clonefile shares the blocks of src with dst by FICLONE, which XFS and Btrfs do in one call; another filesystem says so.
func clonefile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	err = unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	if err == nil {
		return nil
	}
	// The name is freed for the copy that follows, or for a retry.
	err = errors.Join(err, os.Remove(dst))
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("%w: %w", err, errors.ErrUnsupported)
	}

	return err
}
