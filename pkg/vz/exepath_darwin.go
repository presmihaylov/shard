//go:build darwin && cgo

package vz

/*
#include <libproc.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// executable is the path the kernel holds for the file pid runs from, which it keeps across a rename over that path.
func executable(pid int) (string, error) {
	buf := make([]byte, C.PROC_PIDPATHINFO_MAXSIZE)
	n, err := C.proc_pidpath(C.int(pid), unsafe.Pointer(&buf[0]), C.uint32_t(len(buf)))
	if n <= 0 {
		return "", fmt.Errorf("proc_pidpath: %w", err)
	}

	return string(buf[:n]), nil
}
