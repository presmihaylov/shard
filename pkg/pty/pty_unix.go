//go:build linux || darwin

package pty

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// inputPoll bounds how long a cancel goes unseen while the input has nothing to read.
const inputPoll = 50 * time.Millisecond

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

func readPassword(ctx context.Context, f *os.File) (line []byte, err error) {
	fd := int(f.Fd())

	previous, err := unix.IoctlGetTermios(fd, getTermios)
	if err != nil {
		return nil, fmt.Errorf("read the terminal settings of %s: %w", f.Name(), err)
	}

	quiet := *previous
	quiet.Lflag &^= unix.ECHO
	// ISIG stays on, so Ctrl-C at the prompt cancels the verb rather than becoming part of the value.
	quiet.Lflag |= unix.ICANON | unix.ISIG
	quiet.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, setTermios, &quiet); err != nil {
		return nil, fmt.Errorf("turn the echo of %s off: %w", f.Name(), err)
	}
	defer func() {
		// A prompt that ends early discards what was typed, so a half-typed secret never reaches the next reader.
		if err != nil {
			if flushErr := flushInput(fd); flushErr != nil {
				err = errors.Join(err, fmt.Errorf("discard the unread input of %s: %w", f.Name(), flushErr))
			}
		}
		if restoreErr := unix.IoctlSetTermios(fd, setTermios, previous); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore the terminal settings of %s: %w", f.Name(), restoreErr))
		}
	}()

	return readPasswordLine(Input(ctx, f))
}

// awaitInput waits in short selects rather than one blocking read, so a cancel is seen and nothing is left reading f.
func awaitInput(ctx context.Context, f *os.File) error {
	fd := int(f.Fd())
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		var ready unix.FdSet
		ready.Set(fd)
		timeout := unix.NsecToTimeval(inputPoll.Nanoseconds())
		n, err := unix.Select(fd+1, &ready, nil, nil, &timeout)
		// A SIGWINCH or the SIGINT that cancels ctx interrupts the select, and the loop looks again.
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("wait for input on %s: %w", f.Name(), err)
		}
		if n > 0 {
			return nil
		}
	}
}
