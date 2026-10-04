//go:build linux

package launch

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// defaultPath is the OCI image spec default, which execvp falls back to as well when the env names no PATH.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Shim waits for the host trace so only the kernel can confirm the command's execve.
func Shim(argv []string) error { return shim(argv, true) }

// shim is Shim, and undumpable false keeps a test that is not root able to trace it.
func shim(argv []string, undumpable bool) error {
	if len(argv) == 0 {
		return errors.New("the launch shim has no command")
	}

	unix.CloseOnExec(fd)
	// Only a tracer with CAP_SYS_PTRACE reaches a non-dumpable process, and the execve resets the flag for the command.
	if undumpable {
		if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
			return fmt.Errorf("make the launch shim non-dumpable: %w", err)
		}
	}

	if _, err := unix.Write(fd, []byte{ready}); err != nil {
		return fmt.Errorf("tell the host the launch shim is ready: %w", err)
	}

	buf := make([]byte, 1)
	n, err := unix.Read(fd, buf)
	if err != nil {
		return fmt.Errorf("wait for the host's trace: %w", err)
	}
	if n != 1 || buf[0] != proceed {
		return errors.New("the host ended the launch before its trace")
	}

	errno := execvp(argv)
	if _, err := unix.Write(fd, record(errno)); err != nil {
		return errors.Join(&NotStartedError{Errno: errno}, fmt.Errorf("report the errno to the host: %w", err))
	}

	return &NotStartedError{Errno: errno}
}

// execvp omits the shell fallback because runc treats an unrecognized executable as a refusal.
func execvp(argv []string) syscall.Errno {
	env := os.Environ()
	name := argv[0]
	if strings.Contains(name, "/") {
		return errnoOf(unix.Exec(name, argv, env))
	}

	dirs, ok := os.LookupEnv("PATH")
	if !ok {
		dirs = defaultPath
	}

	denied := false
	for dir := range strings.SplitSeq(dirs, ":") {
		if dir == "" {
			continue
		}

		errno := errnoOf(unix.Exec(path.Join(dir, name), argv, env))
		if errno == unix.EACCES {
			denied = true

			continue
		}
		if errno != unix.ENOENT && errno != unix.ENOTDIR {
			return errno
		}
	}

	if denied {
		return unix.EACCES
	}

	return unix.ENOENT
}

// errnoOf is the errno of a failed execve; Go returns every one as a bare Errno.
func errnoOf(err error) syscall.Errno {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}

	return unix.ENOEXEC
}
