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

// Shim waits for the host trace so only the kernel can confirm the command's execve. args is what Args put after Mode.
func Shim(args []string) error { return shim(args, true) }

// shim is Shim, and undumpable false keeps a test that is not root able to trace it.
func shim(args []string, undumpable bool) error {
	if len(args) < 2 {
		return errors.New("the launch shim has no work directory and command")
	}
	workDir, argv := args[0], args[1:]

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

	// The chdir runs as the exec's user, as the runtime's own would, and a refusal is never the command's.
	if err := unix.Chdir(workDir); err != nil {
		return report(unentered, errnoOf(err))
	}

	return report(failed, execvp(argv))
}

// report sends the host the errno that ended the launch and returns it as this process's own failure.
func report(kind byte, errno syscall.Errno) error {
	notStarted := &NotStartedError{Errno: errno, Chdir: kind == unentered}
	if _, err := unix.Write(fd, record(kind, errno)); err != nil {
		return errors.Join(notStarted, fmt.Errorf("report the errno to the host: %w", err))
	}

	return notStarted
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

// errnoOf is the errno of a failed execve or chdir; Go returns every one as a bare Errno.
func errnoOf(err error) syscall.Errno {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}

	return unix.ENOEXEC
}
