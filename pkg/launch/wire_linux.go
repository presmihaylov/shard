//go:build linux

package launch

import (
	"strconv"
	"syscall"
)

// fd is where the runtime puts the shim's end of the channel, the first preserved fd.
const fd = 3

// The channel carries one byte each way and at most one errno record back, and nothing else.
const (
	ready   byte = 'R'
	proceed byte = 'G'
	failed  byte = 'E'
)

// record is the shim's report of the errno that ended its search.
func record(errno syscall.Errno) []byte {
	return strconv.AppendUint([]byte{failed}, uint64(errno), 10)
}

// parseRecord reads an errno record; anything else is no record at all.
func parseRecord(blob []byte) syscall.Errno {
	if len(blob) < 2 || blob[0] != failed {
		return 0
	}

	errno, err := strconv.Atoi(string(blob[1:]))
	if err != nil || errno <= 0 || errno > 4095 {
		return 0
	}

	return syscall.Errno(errno)
}
