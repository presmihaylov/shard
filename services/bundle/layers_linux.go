package bundle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// opaqueXattr marks a directory overlayfs made in an upper layer over one it removed, which hides every copy below it.
const opaqueXattr = "trusted.overlay.opaque"

// opaqueDir reads the mark off the directory's own handle, which neither follows a link nor waits on a fifo.
func opaqueDir(layer *os.Root, rel string) (bool, error) {
	dir, err := layer.OpenFile(rel, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return false, err
	}
	defer dir.Close()

	value := make([]byte, 1)
	n, err := unix.Fgetxattr(int(dir.Fd()), opaqueXattr, value)
	if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s of %s: %w", opaqueXattr, filepath.Join(layer.Name(), rel), err)
	}

	return n == 1 && value[0] == 'y', nil
}
