//go:build darwin

package pty

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ptmx is a var so a test can point open at a file that is no multiplexer, which is the only way to
// reach the failure path below.
var ptmx = "/dev/ptmx"

func open() (*Pty, error) {
	master, err := os.OpenFile(ptmx, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", ptmx, err)
	}

	pair, err := replicaOf(master)
	if err != nil {
		return nil, errors.Join(err, master.Close())
	}

	return pair, nil
}

// replicaOf is grantpt, unlockpt and ptsname, which Darwin spells as three ioctls on the master.
func replicaOf(master *os.File) (*Pty, error) {
	fd := int(master.Fd())

	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		return nil, fmt.Errorf("grant the pseudo terminal: %w", err)
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		return nil, fmt.Errorf("unlock the pseudo terminal: %w", err)
	}
	path, err := ptsname(fd)
	if err != nil {
		return nil, err
	}

	// O_NOCTTY: the replica becomes a controlling terminal only for the guest process, never for shard.
	replica, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return &Pty{Master: master, Replica: replica}, nil
}

// ptsname fills the 128-byte name the C library hands the same ioctl; x/sys has no typed helper for it.
func ptsname(fd int) (string, error) {
	var name [128]byte
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))) //nolint:gosec // the ioctl fills a buffer
	if errno != 0 {
		return "", fmt.Errorf("read the pseudo terminal name: %w", errno)
	}

	return unix.ByteSliceToString(name[:]), nil
}

// The termios ioctls have one name per kernel; the shared driver reads them through these.
const (
	getTermios = unix.TIOCGETA
	setTermios = unix.TIOCSETA
)
