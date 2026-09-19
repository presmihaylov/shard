//go:build !linux

package main

import (
	"errors"

	"github.com/presmihaylov/shard/services/supervisor"
)

var errNotLinux = errors.New("a root disk needs Linux")

func bootGuest(string) error { return errNotLinux }

func dropPtrace() error { return nil }

func applyAddress(supervisor.Address) error { return errNotLinux }

func powerOff() error { return nil }
