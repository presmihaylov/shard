package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Exchange swaps what is at a with what is at b in one step, so a reader of either path never finds neither; a filesystem without it says so.
func Exchange(a, b string) error {
	if err := exchange(a, b); err != nil {
		return fmt.Errorf("exchange %s and %s: %w", a, b, err)
	}

	return nil
}

// SwapDir installs src at dst and drops what dst held: a rename when dst holds nothing, an exchange when it does, so no cut leaves dst absent.
func SwapDir(src, dst string) error {
	_, err := os.Stat(dst)
	if errors.Is(err, fs.ErrNotExist) {
		return os.Rename(src, dst)
	}
	if err != nil {
		return err
	}
	if err := Exchange(src, dst); err != nil {
		return err
	}

	// The exchange left what dst held at src, which the caller owns and drops.
	return os.RemoveAll(src)
}
