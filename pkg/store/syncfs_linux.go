package store

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// SyncFS makes every write on the filesystem that holds dir durable, which a tree of many files needs in one call.
func SyncFS(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer d.Close()

	if err := unix.Syncfs(int(d.Fd())); err != nil {
		return fmt.Errorf("syncfs %s: %w", dir, err)
	}

	return nil
}
