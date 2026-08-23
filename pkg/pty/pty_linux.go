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

func resize(f *os.File, size Size) error {
	if err := unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: size.Rows, Col: size.Cols}); err != nil {
		return fmt.Errorf("set the window size of %s: %w", f.Name(), err)
	}

	return nil
}

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}

	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)

	return err == nil
}

func sizeOf(f *os.File) (Size, error) {
	winsize, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return Size{}, fmt.Errorf("read the window size of %s: %w", f.Name(), err)
	}

	return Size{Rows: winsize.Row, Cols: winsize.Col}, nil
}

func makeRaw(f *os.File) (Restore, error) {
	fd := int(f.Fd())

	previous, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, fmt.Errorf("read the terminal settings of %s: %w", f.Name(), err)
	}

	raw := *previous
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	// ISIG off is the whole point: Ctrl-C becomes a byte the guest reads, not a signal shard catches.
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, fmt.Errorf("put %s into raw mode: %w", f.Name(), err)
	}

	return func() error {
		if err := unix.IoctlSetTermios(fd, unix.TCSETS, previous); err != nil {
			return fmt.Errorf("restore the terminal settings of %s: %w", f.Name(), err)
		}

		return nil
	}, nil
}
