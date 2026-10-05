// Package filemode names the type of a file in words, for an error a person reads.
package filemode

import "io/fs"

// Name says what a file is, since an fs.FileMode prints its type as a mode string such as "p---------".
func Name(mode fs.FileMode) string {
	switch {
	case mode.IsRegular():
		return "regular file"
	case mode&fs.ModeDir != 0:
		return "directory"
	case mode&fs.ModeSymlink != 0:
		return "symbolic link"
	case mode&fs.ModeNamedPipe != 0:
		return "named pipe"
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeCharDevice != 0:
		return "character device"
	case mode&fs.ModeDevice != 0:
		return "block device"
	}

	return "file of an unknown type"
}
