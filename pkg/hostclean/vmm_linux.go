//go:build integration && linux

package hostclean

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// openPidfd is unix.PidfdOpen, a variable so a test can refuse the pin.
var openPidfd = unix.PidfdOpen

// pinVMM holds pid by a pidfd and reads its --api-sock again through that pin, so the kill reaches the process Find matched or none.
func pinVMM(pid int, sock string) (Leftover, bool, error) {
	fd, err := openPidfd(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return Leftover{}, false, nil
	}
	if err != nil {
		refused := fmt.Errorf("pin the vmm %d: %w, so it was not signalled", pid, err)

		return Leftover{What: "the firecracker vmm", Path: sock, remove: func() error { return refused }}, true, nil
	}
	pin := os.NewFile(uintptr(fd), "pidfd "+strconv.Itoa(pid))

	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	// A pid stays with its process until the reap, so a read the pinned process outlives was its own.
	if err == nil {
		err = unix.PidfdSendSignal(fd, 0, nil, 0)
	}
	if gone(err) {
		return Leftover{}, false, pin.Close()
	}
	if err != nil {
		return Leftover{}, false, errors.Join(fmt.Errorf("read the command line of %d through its pin: %w", pid, err), pin.Close())
	}
	if apiSocket(strings.Split(string(cmdline), "\x00")) != sock {
		return Leftover{}, false, pin.Close()
	}

	return Leftover{What: "the firecracker vmm", Path: sock, pin: pin, remove: killVMM(pin, fd, pid)}, true, nil
}

// killVMM signals through the pin, never the pid, and waits for the exit, because only then are the cgroup and the tap free.
func killVMM(pin *os.File, fd, pid int) func() error {
	return func() error {
		err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		if errors.Is(err, unix.ESRCH) {
			return pin.Close()
		}
		if err != nil {
			return errors.Join(fmt.Errorf("kill the vmm %d: %w", pid, err), pin.Close())
		}

		return errors.Join(awaitExit(fd, pid), pin.Close())
	}
}

// awaitExit waits for the pidfd to read ready, which it does once the vmm exits, a zombie included.
func awaitExit(fd, pid int) error {
	deadline := time.Now().Add(killGrace)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("the vmm %d still runs %s after a SIGKILL", pid, killGrace)
		}
		n, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, int(left.Milliseconds())) //nolint:gosec // G115: the kernel hands out an fd that fits an int32
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("wait for the vmm %d: %w", pid, err)
		}
		if n > 0 {
			return nil
		}
	}
}
