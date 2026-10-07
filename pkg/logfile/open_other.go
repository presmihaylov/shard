//go:build !linux

package logfile

import (
	"errors"
	"os"
	"syscall"
)

// openRegular neither follows a link nor waits on a fifo; off Linux the daemon writes the logs from the guest's events, so no guest reaches the path.
func openRegular(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := requireRegular(f, path); err != nil {
		return nil, errors.Join(err, f.Close())
	}

	return f, nil
}
