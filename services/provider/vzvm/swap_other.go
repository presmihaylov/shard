//go:build !darwin

package vzvm

import "os"

// swapDir puts src at dst; only the fake shim runs here, so the moment between the two renames is a test's, not a sandbox's.
func swapDir(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}

	return os.Rename(src, dst)
}
