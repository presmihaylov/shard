//go:build linux

package vsock

import (
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// listener wraps the socket in an os.File, so Accept parks on the runtime poller instead of a thread.
type listener struct {
	file *os.File
	addr Addr
}

func listen(port uint32) (net.Listener, error) {
	// Non-blocking from the start, so os.NewFile registers the fd with the poller and deadlines work.
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open a vsock socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		return nil, errors.Join(fmt.Errorf("bind vsock port %d: %w", port, err), unix.Close(fd))
	}
	if err := unix.Listen(fd, unix.SOMAXCONN); err != nil {
		return nil, errors.Join(fmt.Errorf("listen on vsock port %d: %w", port, err), unix.Close(fd))
	}

	return &listener{file: os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d", port)), addr: Addr{Port: port}}, nil
}

func (l *listener) Accept() (net.Conn, error) {
	raw, err := l.file.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("accept on %s: %w", l.addr, err)
	}

	var (
		fd     int
		sa     unix.Sockaddr
		accept error
	)
	err = raw.Read(func(s uintptr) bool {
		fd, sa, accept = unix.Accept4(int(s), unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)

		return !errors.Is(accept, unix.EAGAIN)
	})
	if err != nil {
		return nil, fmt.Errorf("accept on %s: %w", l.addr, err)
	}
	if accept != nil {
		return nil, fmt.Errorf("accept on %s: %w", l.addr, accept)
	}

	remote := Addr{}
	if vm, ok := sa.(*unix.SockaddrVM); ok {
		remote.Port = vm.Port
	}

	return &conn{File: os.NewFile(uintptr(fd), l.addr.String()), local: l.addr, remote: remote}, nil
}

func (l *listener) Close() error   { return l.file.Close() }
func (l *listener) Addr() net.Addr { return l.addr }

// conn is one accepted stream. os.File already reads, writes, closes and keeps deadlines on a pollable fd.
type conn struct {
	*os.File
	local, remote Addr
}

func (c *conn) LocalAddr() net.Addr  { return c.local }
func (c *conn) RemoteAddr() net.Addr { return c.remote }

// CloseWrite sends the host EOF and keeps reading, which is how a session says the output is over.
func (c *conn) CloseWrite() error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}

	var shut error
	if err := raw.Control(func(fd uintptr) { shut = unix.Shutdown(int(fd), unix.SHUT_WR) }); err != nil {
		return err
	}

	return shut
}

var _ net.Conn = (*conn)(nil)
