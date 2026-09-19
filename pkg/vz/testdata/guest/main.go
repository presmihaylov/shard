// The fixture PID 1: it answers every vsock connection on the control port with its pid, echoes it on the console, then holds the line.
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
	console, err := openConsole()
	if err != nil {
		return err
	}
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
		line := fmt.Appendf(nil, "pid=%d\n", os.Getpid())
		if _, err := unix.Write(conn, line); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		if _, err := console.Write(line); err != nil {
			return fmt.Errorf("console: %w", err)
		}
		if err := unix.Close(conn); err != nil {
			return fmt.Errorf("close: %w", err)
		}
	}
}

// An initramfs has no /dev until someone mounts it; hvc0 is the port the host reads, and /dev/console does not reach it on macOS 26.
func openConsole() (*os.File, error) {
	if err := os.MkdirAll("/dev", 0o755); err != nil {
		return nil, fmt.Errorf("mkdir /dev: %w", err)
	}
	if err := unix.Mount("devtmpfs", "/dev", "devtmpfs", 0, ""); err != nil {
		return nil, fmt.Errorf("mount /dev: %w", err)
	}
	console, err := os.OpenFile("/dev/hvc0", os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open the console: %w", err)
	}

	return console, nil
}
