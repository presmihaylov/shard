// Package launch uses the kernel exec event because the runtime's socket copy makes EOF inconclusive.
package launch

import (
	"errors"
	"fmt"
	"syscall"
)

// Mode is the shim's first argument to shard-init; the work directory and the command follow it.
const Mode = "launch"

// ErrNoShim is a runtime that ended before the shim said it was ready, so the command never ran.
var ErrNoShim = errors.New("the runtime ended before the launch shim was ready")

// ErrTraceDenied is a host whose ptrace policy refuses the trace that proves a launch; nothing falls back.
var ErrTraceDenied = errors.New("the host refused the trace that proves an exec launched")

// Args is the process a runtime starts for one launch. The shim enters workDir itself, so a missing one is its own refusal.
func Args(shim, workDir string, argv []string) []string {
	return append([]string{shim, Mode, workDir}, argv...)
}

// NotStartedError is a command whose execve never took. Errno is the shim's own report, zero when it ended without one.
type NotStartedError struct {
	Errno syscall.Errno
	// Chdir is an errno from entering the work directory, before any search for the command.
	Chdir bool
}

func (e *NotStartedError) Error() string { return e.Reason() }

// Reason is the kernel's words for the errno, so no guest text reaches the message.
func (e *NotStartedError) Reason() string {
	if e.Errno == 0 {
		return "the command ended before its exec took, with no reason reported"
	}
	if e.Chdir {
		return "the work directory cannot be entered: " + e.Errno.Error()
	}

	return e.Errno.Error()
}

// NotFound separates a command that is not there from one that is there and cannot run, as a shell's 127 and 126 do.
func (e *NotStartedError) NotFound() bool { return e.Errno == syscall.ENOENT && !e.Chdir }

// WorkDirReason names the directory a command could not start in, so the refusal never reads as the command's.
func WorkDirReason(dir string, errno syscall.Errno) string {
	switch errno {
	case syscall.ENOENT:
		return fmt.Sprintf("the work directory %q does not exist", dir)
	case syscall.ENOTDIR:
		return fmt.Sprintf("the work directory %q is not a directory", dir)
	}

	return fmt.Sprintf("the work directory %q cannot be entered: %v", dir, errno)
}
