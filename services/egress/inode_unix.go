//go:build unix

package egress

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// inodeOf is what tells a rotation apart from an append: the name is the same and the file is not.
func inodeOf(info fs.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}

	return stat.Ino, true
}

func inode(file *os.File) (uint64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", file.Name(), err)
	}

	ino, ok := inodeOf(info)
	if !ok {
		return 0, fmt.Errorf("stat %s: the host gave no inode", file.Name())
	}

	return ino, nil
}
