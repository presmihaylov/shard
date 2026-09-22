// Package reflink asks a filesystem whether it clones a file by sharing its blocks, and clones one when it does.
package reflink

import "errors"

// ErrNotLinux is what every call answers off Linux: the ioctl and statfs magics exist nowhere else.
var ErrNotLinux = errors.New("reflink: linux only")

// Filesystem is what the kernel holds a directory on, and whether a clone there shares blocks.
type Filesystem struct {
	Type    string
	Reflink bool
}

// Probe clones one small file under dir and reports whether the filesystem there did it by reference.
func Probe(dir string) (Filesystem, error) {
	return probe(dir)
}

// Clone makes dst share every block of src; an existing dst is replaced.
func Clone(src, dst string) error {
	return clone(src, dst)
}
