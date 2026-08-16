// Package store writes files on disk the way a crash-safe manager must: never half a file.
package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFile lands data at path or leaves what was there. A reader never sees a partial file.
func WriteFile(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())

	if err := writeAndSync(tmp, data, perm); err != nil {
		return errors.Join(err, tmp.Close())
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmp.Name(), path, err)
	}

	return SyncDir(dir)
}

func writeAndSync(f *os.File, data []byte, perm fs.FileMode) error {
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", f.Name(), err)
	}

	// Chmod, not the O_CREATE mode: CreateTemp fixes the mode at 0600 and umask would trim ours anyway.
	if err := f.Chmod(perm); err != nil {
		return fmt.Errorf("chmod %s: %w", f.Name(), err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", f.Name(), err)
	}

	return nil
}

// SyncDir makes an entry that appeared in dir durable, which the file's own fsync does not do.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer d.Close()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}

	return nil
}
