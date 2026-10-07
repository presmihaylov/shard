//go:build !linux

package bundle

import "os"

// opaqueDir is never true off Linux, where no overlayfs made the layers.
func opaqueDir(_ *os.Root, _ string) (bool, error) {
	return false, nil
}
