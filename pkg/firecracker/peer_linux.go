//go:build linux

package firecracker

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// peerPID is the process behind a unix socket connection, which the kernel attests.
func peerPID(conn net.Conn) (int, error) {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return 0, fmt.Errorf("a %T carries no peer credentials", conn)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return 0, err
	}

	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	if cred.Pid <= 0 {
		return 0, errors.New("the peer reported no pid")
	}

	return int(cred.Pid), nil
}
