// The fixture PID 1: it answers every vsock connection on the control port with its pid, then holds the line.
package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const port = 5000

func main() {
	if err := serve(); err != nil {
		fmt.Fprintln(os.Stderr, "guest:", err)
	}
	// PID 1 exiting panics the kernel, so a failure parks here and the host times out instead.
	select {}
}

func serve() error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		return fmt.Errorf("bind: %w", err)
	}
	if err := unix.Listen(fd, 8); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	for {
		conn, _, err := unix.Accept(fd)
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		if _, err := unix.Write(conn, []byte(fmt.Sprintf("pid=%d\n", os.Getpid()))); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		if err := unix.Close(conn); err != nil {
			return fmt.Errorf("close: %w", err)
		}
	}
}
