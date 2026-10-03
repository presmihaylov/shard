//go:build !linux && !(darwin && cgo)

package pidpin

import (
	"errors"
	"syscall"
)

var errUnsupported = errors.New("a process is pinned by a pidfd on linux, or by its audit token on darwin built with cgo")

type handle struct{}

func open(int) (handle, error) { return handle{}, errUnsupported }

func signal(handle, syscall.Signal) error { return errUnsupported }

func release(handle) error { return nil }
