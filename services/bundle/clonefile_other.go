//go:build !darwin

package bundle

import "errors"

// clonefile has no block-sharing copy here, so CloneRootDisk falls back to a plain copy.
func clonefile(_, _ string) error {
	return errors.ErrUnsupported
}
