package vzvm

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// swapDir puts src at dst in one step, so no moment leaves dst empty, and drops what dst held.
func swapDir(src, dst string) error {
	if _, err := os.Stat(dst); errors.Is(err, fs.ErrNotExist) {
		return os.Rename(src, dst)
	}
	if err := unix.RenameatxNp(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_SWAP); err != nil {
		return err
	}

	return os.RemoveAll(src)
}
