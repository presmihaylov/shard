//go:build !linux

package kmsg

import "errors"

// Read fails: only Linux has a kernel log ring at /dev/kmsg.
func Read() ([]Line, error) {
	return nil, errors.New("the kernel log can be read on Linux only")
}
