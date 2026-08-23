//go:build !linux

package pty

import "os"

func open() (*Pty, error) { return nil, ErrNotLinux }

func resize(*os.File, Size) error { return ErrNotLinux }

func isTerminal(*os.File) bool { return false }

func sizeOf(*os.File) (Size, error) { return Size{}, ErrNotLinux }

func makeRaw(*os.File) (Restore, error) { return nil, ErrNotLinux }
