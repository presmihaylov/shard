package store

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Free is the space an unprivileged writer can still take on the filesystem that holds dir.
func Free(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}

	return int64(st.Bavail) * int64(st.Bsize), nil //nolint:gosec // G115: no disk holds 8 EiB free
}

// WriteReserve fills path with size bytes of real blocks, never a sparse hole, so its delete gives them all back.
func WriteReserve(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := fill(f, size); err != nil {
		return errors.Join(err, f.Close(), os.Remove(path))
	}
	if err := f.Close(); err != nil {
		return errors.Join(fmt.Errorf("close %s: %w", path, err), os.Remove(path))
	}

	return SyncDir(filepath.Dir(path))
}

func fill(f *os.File, size int64) error {
	// Random bytes, not zeros, so a compressing filesystem cannot keep them in less than size.
	chunk := make([]byte, 1<<20)
	if _, err := rand.Read(chunk); err != nil {
		return fmt.Errorf("read random bytes for %s: %w", f.Name(), err)
	}

	for written := int64(0); written < size; {
		n := min(int64(len(chunk)), size-written)
		if _, err := f.Write(chunk[:n]); err != nil {
			return fmt.Errorf("write %s: %w", f.Name(), err)
		}
		written += n
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", f.Name(), err)
	}

	return nil
}
