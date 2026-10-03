// Package store writes files on disk the way a crash-safe manager must: never half a file.
package store

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
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

// WriteFileIfChanged is WriteFile that leaves a file already holding data with perm alone, so a full disk fails no write that changes nothing.
func WriteFileIfChanged(path string, data []byte, perm fs.FileMode) error {
	same, err := holds(path, data, perm)
	if err != nil {
		return err
	}
	if same {
		return nil
	}

	return WriteFile(path, data, perm)
}

// holds neither follows a symlink nor waits on a fifo: neither holds data, so the write replaces it.
func holds(path string, data []byte, perm fs.FileMode) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != perm || info.Size() != int64(len(data)) {
		return false, nil
	}
	held, err := io.ReadAll(f)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	return bytes.Equal(held, data), nil
}

// WriteFileIn is WriteFile with name resolved inside root, so no symlink under root leads the write out of it.
func WriteFileIn(root *os.Root, name string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(name)
	tmpName := filepath.Join(dir, "."+filepath.Base(name)+".tmp-"+rand.Text())

	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}

	if err := writeAndSync(tmp, data, perm); err != nil {
		return errors.Join(err, tmp.Close(), root.Remove(tmpName))
	}

	if err := tmp.Close(); err != nil {
		return errors.Join(fmt.Errorf("close %s: %w", tmpName, err), root.Remove(tmpName))
	}

	if err := root.Rename(tmpName, name); err != nil {
		return errors.Join(fmt.Errorf("rename %s to %s: %w", tmpName, name, err), root.Remove(tmpName))
	}

	d, err := root.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	if err := d.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync %s: %w", dir, err), d.Close())
	}

	return d.Close()
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
