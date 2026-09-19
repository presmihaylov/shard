//go:build linux

package pty

import (
	"errors"
	"fmt"
	"os"

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
		// Join, not a second %w: a close that went fine is no operand, and a nil one renders as one.
		return nil, errors.Join(err, master.Close())
	}

	return pair, nil
}

func replicaOf(master *os.File) (*Pty, error) {
	fd := int(master.Fd())

	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return nil, fmt.Errorf("unlock the pseudo terminal: %w", err)
	}

	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		return nil, fmt.Errorf("read the pseudo terminal number: %w", err)
	}

	path := fmt.Sprintf("/dev/pts/%d", n)
	// O_NOCTTY: the replica becomes a controlling terminal only for the guest process, never for shard.
	replica, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return &Pty{Master: master, Replica: replica}, nil
}

// The termios ioctls have one name per kernel; the shared driver reads them through these.
const (
	getTermios = unix.TCGETS
	setTermios = unix.TCSETS
)
