package logfile

import (
	"fmt"
	"os"

	"github.com/presmihaylov/shard/pkg/filemode"
)

// Open opens a log for a read: guest root writes in the directory that holds it, so a link or a special file it put there is refused.
func Open(path string) (*os.File, error) {
	return openRegular(path, os.O_RDONLY)
}

// requireRegular refuses anything but a regular file, whose read can neither block nor reach a driver.
func requireRegular(f *os.File, path string) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is a %s, and it must be a regular file", path, filemode.Name(info.Mode()))
	}

	return nil
}
