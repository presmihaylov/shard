//go:build !linux

// Package diskimage makes and mounts a sparse ext4 image, so a directory tree is bounded by a file's size.
package diskimage

import "errors"

// ErrNotLinux keeps a developer Mac honest: an ext4 image is made and mounted by the Linux kernel alone.
var ErrNotLinux = errors.New("a sandbox disk needs mkfs.ext4 and a loop mount, which only Linux has")

func Create(path string, size int64) error { return ErrNotLinux }

func Mount(image, dir string) error { return ErrNotLinux }

func Unmount(image, dir string) error { return ErrNotLinux }

func Mounted(image, dir string) (bool, error) { return false, ErrNotLinux }
