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

// SwapDir installs src at dst, by a rename when dst holds nothing and an exchange when it does, so no cut
// leaves dst absent. An error means dst still holds what it held, and a success leaves that at src, which
// the caller drops: a failure to drop it is not a failure to install.
func SwapDir(src, dst string) error {
	_, err := os.Stat(dst)
	if errors.Is(err, fs.ErrNotExist) {
		return os.Rename(src, dst)
	}
	if err != nil {
		return err
	}

	return Exchange(src, dst)
}
