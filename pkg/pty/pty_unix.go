//go:build linux || darwin

package pty

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

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

	_, err := unix.IoctlGetTermios(int(f.Fd()), getTermios)

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

	previous, err := unix.IoctlGetTermios(fd, getTermios)
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

	if err := unix.IoctlSetTermios(fd, setTermios, &raw); err != nil {
		return nil, fmt.Errorf("put %s into raw mode: %w", f.Name(), err)
	}

	return func() error {
		if err := unix.IoctlSetTermios(fd, setTermios, previous); err != nil {
			return fmt.Errorf("restore the terminal settings of %s: %w", f.Name(), err)
		}

		return nil
	}, nil
}
