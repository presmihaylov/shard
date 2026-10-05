// Package pty drives the host's pseudo terminals. It knows ioctls and termios, and nothing about
// sandboxes: a guest gets a terminal only because the process that talks to it holds one end of this.
package pty

import (
	"context"
	"errors"
	"io"
	"os"
)

// ErrUnsupported names the hosts with a driver; it is not models.ErrUnsupported, since a missing kernel is not a refused verb.
var ErrUnsupported = errors.New("a pseudo terminal needs Linux or macOS")

// Size is a terminal window in character cells. A guest full-screen program draws to it.
type Size struct {
	Rows uint16
	Cols uint16
}

// Restore puts a terminal back the way MakeRaw found it. Call it on every exit path.
type Restore func() error

// Pty is one host pseudo terminal pair. The guest process gets the replica, and shard keeps the master.
type Pty struct {
	Master  *os.File
	Replica *os.File
}

// Open allocates a pair. The caller closes both, and closing the master ends the session.
func Open() (*Pty, error) { return open() }

// Close drops both ends. It is safe to call after the replica was already handed over and closed.
func (p *Pty) Close() error {
	return errors.Join(closeFile(p.Replica), closeFile(p.Master))
}

// Resize sets the window the guest sees. Writing it to the master raises SIGWINCH on the replica side.
func (p *Pty) Resize(size Size) error { return resize(p.Master, size) }

// IsTerminal reports whether f is a terminal, which is what a -t exec refuses to run without.
func IsTerminal(f *os.File) bool { return isTerminal(f) }

// SizeOf reads the window size of a terminal.
func SizeOf(f *os.File) (Size, error) { return sizeOf(f) }

// ReadPassword reads one line with the echo off, so a secret lands on no screen; a cancel ends it with the echo back on.
func ReadPassword(ctx context.Context, f *os.File) ([]byte, error) { return readPassword(ctx, f) }

// Input reads f only once it has something to read, so a cancel ends a read that would block and parks no goroutine.
func Input(ctx context.Context, f *os.File) io.Reader { return input{ctx: ctx, f: f} }

type input struct {
	ctx context.Context
	f   *os.File
}

func (i input) Read(p []byte) (int, error) {
	if err := awaitInput(i.ctx, i.f); err != nil {
		return 0, err
	}

	return i.f.Read(p)
}

// readPasswordLine is the line discipline of x/term's ReadPassword, read through Input.
func readPasswordLine(r io.Reader) ([]byte, error) {
	var b [1]byte
	var line []byte
	for {
		n, err := r.Read(b[:])
		if n > 0 {
			switch b[0] {
			case '\b':
				if len(line) > 0 {
					line = line[:len(line)-1]
				}
			case '\n':
				return line, nil
			case '\r':
			default:
				line = append(line, b[0])
			}

			continue
		}
		if errors.Is(err, io.EOF) && len(line) > 0 {
			return line, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// MakeRaw hands every keystroke through untouched, so Ctrl-C reaches the guest instead of shard.
func MakeRaw(f *os.File) (Restore, error) { return makeRaw(f) }

func closeFile(f *os.File) error {
	if f == nil {
		return nil
	}

	return f.Close()
}
