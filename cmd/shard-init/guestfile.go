package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"github.com/presmihaylov/shard/pkg/store"
)

// writeGuestFile is how the guest lays down a file it owns, so a start of a sandbox whose disk is full still succeeds.
func writeGuestFile(path string, data []byte, perm fs.FileMode) error {
	return writeOnFullDisk(store.WriteFileIfChanged, path, data, perm)
}

// writeOnFullDisk falls back to the blocks path already holds only when replace finds no block to write a new file into.
func writeOnFullDisk(replace func(string, []byte, fs.FileMode) error, path string, data []byte, perm fs.FileMode) error {
	err := replace(path, data, perm)
	if !errors.Is(err, syscall.ENOSPC) {
		return err
	}
	if rewriteErr := rewriteInPlace(path, data, perm); rewriteErr != nil {
		return fmt.Errorf("the guest disk is full: free space in the source sandbox, then try again: %w", errors.Join(err, rewriteErr))
	}

	return nil
}

// rewriteInPlace overwrites a regular file inside the blocks it already holds, which a full disk still lets it do.
func rewriteInPlace(path string, data []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open %s in place: %w", path, err)
	}
	if err := overwrite(f, data, perm); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}

	return nil
}

func overwrite(f *os.File, data []byte, perm fs.FileMode) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", f.Name(), err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", f.Name())
	}
	// A second name could be a file the user wrote, which an overwrite would change under it.
	if stat.Nlink != 1 {
		return fmt.Errorf("%s has %d names, and an overwrite would change the others", f.Name(), stat.Nlink)
	}
	if held := stat.Blocks * 512; held < int64(len(data)) {
		return fmt.Errorf("%s holds %d bytes of blocks, and %d bytes need more", f.Name(), held, len(data))
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		return fmt.Errorf("write %s in place: %w", f.Name(), err)
	}
	if err := f.Truncate(int64(len(data))); err != nil {
		return fmt.Errorf("truncate %s: %w", f.Name(), err)
	}
	if err := f.Chmod(perm); err != nil {
		return fmt.Errorf("chmod %s: %w", f.Name(), err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", f.Name(), err)
	}

	return nil
}
