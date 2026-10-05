//go:build !linux

package launch

import (
	"context"
	"errors"
	"os"
	"syscall"
)

// errUnsupported is every launch off Linux, where no container runtime runs and ptrace has another shape.
var errUnsupported = errors.New("a launch proof runs on Linux only")

// Channel is one launch; off Linux none can be opened.
type Channel struct{}

func Open() (*Channel, error) { return nil, errUnsupported }

func (c *Channel) Guest() *os.File { return nil }

func (c *Channel) CloseGuest() error { return errUnsupported }

func (c *Channel) Close() error { return errUnsupported }

func (c *Channel) Kill() error { return errUnsupported }

func (c *Channel) Signal(syscall.Signal) error { return errUnsupported }

func (c *Channel) Await(context.Context, func() (int, error)) (int, error) { return 0, errUnsupported }
