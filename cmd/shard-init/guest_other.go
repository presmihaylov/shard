//go:build !linux

package main

import (
	"errors"

	"github.com/presmihaylov/shard/services/supervisor"
)

var errNotLinux = errors.New("a root disk needs Linux")

func bootGuest(guestBoot) error { return errNotLinux }

func confine() error { return nil }

func applyAddress(supervisor.Address) error { return errNotLinux }

// Off Linux shard-init is only the test double over unix sockets, and no guest kernel holds a crng to rekey.
func reseed([]byte) error { return nil }

func powerOff(bool) error { return nil }
