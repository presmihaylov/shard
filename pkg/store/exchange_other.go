//go:build !darwin && !linux

package store

import "errors"

// exchange has no atomic swap here, so every caller of Exchange is refused.
func exchange(_, _ string) error {
	return errors.ErrUnsupported
}
