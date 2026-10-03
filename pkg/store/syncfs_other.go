//go:build !linux

package store

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// SyncFS has no syncfs here, so it flushes every filesystem; a vz guest boots its fsynced disk file, never this tree.
func SyncFS(dir string) error {
	if err := unix.Sync(); err != nil {
		return fmt.Errorf("sync for %s: %w", dir, err)
	}

	return nil
}
