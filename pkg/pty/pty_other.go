//go:build !linux && !darwin

package pty

import "os"

func open() (*Pty, error) { return nil, ErrUnsupported }

func resize(*os.File, Size) error { return ErrUnsupported }

func isTerminal(*os.File) bool { return false }

func sizeOf(*os.File) (Size, error) { return Size{}, ErrUnsupported }

func makeRaw(*os.File) (Restore, error) { return nil, ErrUnsupported }
