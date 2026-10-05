package bundle

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openDatabase proves the file regular over an O_PATH handle before it opens it for a read, so a device node the guest made never reaches its driver (SHARD-548).
func openDatabase(root *os.Root, rel, full string) (*os.File, error) {
	handle, err := root.OpenFile(rel, unix.O_PATH, 0)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	if err := requireDatabase(handle, rel, full); err != nil {
		return nil, err
	}
	// The magic link reopens the inode the handle proved, not whatever the guest has put at the path since.
	f, err := os.Open(fmt.Sprintf("/proc/self/fd/%d", handle.Fd()))
	if err != nil {
		return nil, fmt.Errorf("reopen %s: %w", full, err)
	}

	return f, nil
}
