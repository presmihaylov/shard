package bundle

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openRegular proves the file regular over an O_PATH handle before it opens it for a read, so a device node the guest made never reaches its driver (SHARD-305).
func openRegular(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle := os.NewFile(uintptr(fd), path)
	defer handle.Close()
	if err := requireRegular(handle, path); err != nil {
		return nil, err
	}

	// The magic link reopens the inode the handle proved, not whatever the guest has put at the path since.
	f, err := os.Open(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return nil, fmt.Errorf("reopen %s: %w", path, err)
	}

	return f, nil
}
