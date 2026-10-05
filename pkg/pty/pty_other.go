//go:build !linux && !darwin

package pty

import (
	"context"
	"os"
)

func open() (*Pty, error) { return nil, ErrUnsupported }

func resize(*os.File, Size) error { return ErrUnsupported }

func isTerminal(*os.File) bool { return false }

func sizeOf(*os.File) (Size, error) { return Size{}, ErrUnsupported }

func makeRaw(*os.File) (Restore, error) { return nil, ErrUnsupported }

func readPassword(context.Context, *os.File) ([]byte, error) { return nil, ErrUnsupported }

// awaitInput has no way to wait on f here, so a cancel lands only before the read starts.
func awaitInput(ctx context.Context, _ *os.File) error { return ctx.Err() }
