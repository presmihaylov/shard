// Package memfd makes an anonymous in-memory file whose size no fd to it can change, and tells one apart.
package memfd

import (
	"errors"
	"os"
)

// ErrNotLinux is what Create answers off Linux, where memfd_create and file seals do not exist.
var ErrNotLinux = errors.New("memfd: linux only")

// Create returns a memfd of exactly size bytes, sealed so no fd to it can grow it, shrink it or seal it further.
func Create(name string, size int64) (*os.File, error) {
	return create(name, size)
}

// Fixed reports whether f carries exactly the seals Create adds. A file that takes no seals is not fixed.
func Fixed(f *os.File) (bool, error) {
	return fixed(f)
}
