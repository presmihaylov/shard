// Package pty drives the host's pseudo terminals. It knows ioctls and termios, and nothing about
// sandboxes: a guest gets a terminal only because the process that talks to it holds one end of this.
package pty

import (
	"errors"
	"os"
)

// ErrNotLinux keeps a developer Mac honest. It is not models.ErrUnsupported: a missing kernel is not
// a refused verb.
var ErrNotLinux = errors.New("a pseudo terminal needs Linux")

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

// MakeRaw hands every keystroke through untouched, so Ctrl-C reaches the guest instead of shard.
func MakeRaw(f *os.File) (Restore, error) { return makeRaw(f) }

func closeFile(f *os.File) error {
	if f == nil {
		return nil
	}

	return f.Close()
}
