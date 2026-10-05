//go:build !linux

package bundle

import (
	"errors"
	"os"
	"syscall"
)

// openDatabase neither follows a link nor waits on a fifo; off Linux there is no O_PATH to prove the file before the open.
func openDatabase(root *os.Root, rel, full string) (*os.File, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := requireDatabase(f, rel, full); err != nil {
		return nil, errors.Join(err, f.Close())
	}

	return f, nil
}
