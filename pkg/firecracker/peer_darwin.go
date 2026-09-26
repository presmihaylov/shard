//go:build darwin

package firecracker

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// peerPID is the process behind a unix socket connection, which the kernel attests; a Mac runs no firecracker, but the tests run here.
func peerPID(conn net.Conn) (int, error) {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return 0, fmt.Errorf("a %T carries no peer credentials", conn)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return 0, err
	}

	var pid int
	var pidErr error
	if err := raw.Control(func(fd uintptr) {
		pid, pidErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, err
	}
	if pidErr != nil {
		return 0, pidErr
	}
	if pid <= 0 {
		return 0, errors.New("the peer reported no pid")
	}

	return pid, nil
}
