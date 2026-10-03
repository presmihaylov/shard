//go:build darwin

package daemon

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openLog puts the file at path under stdout and stderr, so every writer the daemon holds, a panic too, lands in it.
func openLog(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open the log %s: %w", path, err)
	}

	for _, fd := range []int{unix.Stdout, unix.Stderr} {
		if err := unix.Dup2(int(f.Fd()), fd); err != nil {
			return errors.Join(fmt.Errorf("point fd %d at the log %s: %w", fd, path, err), f.Close())
		}
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close the log %s: %w", path, err)
	}

	return nil
}
