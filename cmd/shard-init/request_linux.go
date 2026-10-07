package main

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// listenRequests takes the socket name before anything forks, so no guest process can hold it first; a name already held fails the boot.
func listenRequests() (net.Listener, error) {
	l, err := net.Listen("unix", requestAddr)
	if err != nil {
		return nil, fmt.Errorf("listen for process requests on %s: %w", requestAddr, err)
	}

	return l, nil
}

// peerIsRoot admits only root, which the daemon's exec runs as, so a sandbox user who dials the socket starts and stops nothing.
func peerIsRoot(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("a %T is not a unix socket", conn)
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return fmt.Errorf("read the caller's credentials: %w", err)
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) { cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return fmt.Errorf("read the caller's credentials: %w", err)
	}
	if credErr != nil {
		return fmt.Errorf("read the caller's credentials: %w", credErr)
	}
	if cred.Uid != 0 {
		return errors.New("only root sends process requests")
	}

	return nil
}
