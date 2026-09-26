//go:build !darwin && !linux

package bundle

import "errors"

// clonefile has no block-sharing copy here, so CloneRootDisk falls back to a plain copy.
func clonefile(_, _ string) error {
	return errors.ErrUnsupported
}
