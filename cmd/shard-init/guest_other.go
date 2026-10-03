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

func syncDisks() {}

func freezeRoot(*os.File) error { return nil }

func thawRoot(*os.File) error { return nil }

func rootDisk() (*os.File, error) { return nil, nil }

func syncDisk() error { return nil }
