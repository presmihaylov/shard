//go:build !linux

package main

import (
	"errors"
	"os"

	"github.com/presmihaylov/shard/services/supervisor"
)

var errNotLinux = errors.New("a root disk needs Linux")

func bootGuest(guestBoot) error { return errNotLinux }

func confine() (*os.File, error) { return nil, nil }

func applyAddress(supervisor.Address) error { return errNotLinux }

func reseed([]byte) error { return errNotLinux }

func powerOff(bool) error { return nil }

func freezeRoot() error { return nil }

func thawRoot() error { return nil }
