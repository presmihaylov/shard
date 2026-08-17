package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Lock is an advisory flock. The kernel holds it against the open file, so a crash always drops it.
type Lock struct {
	f *os.File
}

// Acquire blocks until it holds path exclusively. It creates the lock file if it does not exist.
// The holder can unlink the file before it releases, and the lock we then win guards an inode that
// path no longer names, which excludes nobody. So take it again until the two are the same file.
func Acquire(path string, perm fs.FileMode) (*Lock, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}

	for {
		f, err := lockFile(path, perm)
		if err != nil {
			return nil, err
		}

		named, err := stillNamed(f, path)
		if err != nil {
			return nil, errors.Join(err, f.Close())
		}

		if named {
			return &Lock{f: f}, nil
		}

		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("close the unlinked lock file %s: %w", path, err)
		}
	}
}

func lockFile(path string, perm fs.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, perm)
	if err != nil {
		return nil, fmt.Errorf("open the lock file %s: %w", path, err)
	}

	if err := flock(f, syscall.LOCK_EX); err != nil {
		return nil, errors.Join(fmt.Errorf("lock %s: %w", path, err), f.Close())
	}

	return f, nil
}

// stillNamed reports whether path names the file we locked, or whether it was unlinked or replaced.
func stillNamed(f *os.File, path string) (bool, error) {
	locked, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("stat the locked file %s: %w", path, err)
	}

	named, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}

	return os.SameFile(locked, named), nil
}

// Path is the lock file the lock is held on, so a caller that removes it needs no second copy.
func (l *Lock) Path() string {
	return l.f.Name()
}

// Release drops the lock. Call it once; a second call reports the closed file.
func (l *Lock) Release() error {
	// Closing the file drops the flock too, so the unlock only makes the order explicit.
	if err := flock(l.f, syscall.LOCK_UN); err != nil {
		return errors.Join(fmt.Errorf("unlock %s: %w", l.f.Name(), err), l.f.Close())
	}

	if err := l.f.Close(); err != nil {
		return fmt.Errorf("close the lock file %s: %w", l.f.Name(), err)
	}

	return nil
}

// A signal handler can interrupt the blocking flock, and that is not a failure to lock.
func flock(f *os.File, how int) error {
	for {
		err := syscall.Flock(int(f.Fd()), how) // #nosec G115 -- a file descriptor is an int already.
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
