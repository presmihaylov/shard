package main

import (
	"fmt"
	"net"
	"os"

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

// peerIsHost admits only root the host's exec entered into the sandbox, so no process born in it, root or not, starts or stops one.
func peerIsHost(conn net.Conn) error {
	cred, err := peerCred(conn)
	if err != nil {
		return err
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", cred.Pid))
	if err != nil {
		return fmt.Errorf("read the caller's status: %w", err)
	}

	return admit(cred.Uid, cred.Pid, status)
}

// peerCred is the uid and pid the kernel recorded when the caller connected.
func peerCred(conn net.Conn) (*unix.Ucred, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("a %T is not a unix socket", conn)
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("read the caller's credentials: %w", err)
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) { cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return nil, fmt.Errorf("read the caller's credentials: %w", err)
	}
	if credErr != nil {
		return nil, fmt.Errorf("read the caller's credentials: %w", credErr)
	}

	return cred, nil
}
