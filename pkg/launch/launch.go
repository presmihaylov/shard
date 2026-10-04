// Package launch proves that a command a container runtime starts reached its own execve. The runtime
// runs a shim with one end of a socketpair as fd 3; the host traces the shim and takes the kernel's
// exec event as the proof, because the runtime holds its own copy of fd 3 and an EOF proves nothing.
package launch

import (
	"errors"
	"strconv"
	"syscall"
)

// Mode is the shim's first argument to shard-init; the command follows it.
const Mode = "launch"

// fd is where the runtime puts the shim's end of the channel, the first preserved fd.
const fd = 3

// The channel carries one byte each way and at most one errno record back, and nothing else.
const (
	ready   byte = 'R'
	proceed byte = 'G'
	failed  byte = 'E'
)

// ErrNoShim is a runtime that ended before the shim said it was ready, so the command never ran.
var ErrNoShim = errors.New("the runtime ended before the launch shim was ready")

// ErrTraceDenied is a host whose ptrace policy refuses the trace that proves a launch; nothing falls back.
var ErrTraceDenied = errors.New("the host refused the trace that proves an exec launched")

// NotStartedError is a command whose execve never took. Errno is the shim's own report, zero when it ended without one.
type NotStartedError struct {
	Errno syscall.Errno
}

func (e *NotStartedError) Error() string { return e.Reason() }

// Reason is the kernel's words for the errno, so no guest text reaches the message.
func (e *NotStartedError) Reason() string {
	if e.Errno == 0 {
		return "the command ended before its exec took, with no reason reported"
	}

	return e.Errno.Error()
}

// NotFound separates a command that is not there from one that is there and cannot run, as a shell's 127 and 126 do.
func (e *NotStartedError) NotFound() bool { return e.Errno == syscall.ENOENT }

// record is the shim's report of the errno that ended its search.
func record(errno syscall.Errno) []byte {
	return strconv.AppendInt([]byte{failed}, int64(errno), 10)
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
