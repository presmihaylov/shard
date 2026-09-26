//go:build linux

package xfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

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

func room(path string) (int64, error) {
	dir := filepath.Dir(path)
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	free := int64(fs.Bavail) * fs.Bsize //nolint:gosec // G115: no disk holds 8 EiB free

	var st unix.Stat_t
	staging := path + stagingSuffix
	err := unix.Stat(staging, &st)
	if errors.Is(err, unix.ENOENT) {
		return free, nil
	}
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", staging, err)
	}

	return free + st.Blocks*512, nil
}
