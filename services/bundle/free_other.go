//go:build !darwin && !linux

package bundle

import (
	"errors"
	"fmt"
)

// freeBytes has no count here, so no disk is admitted: a bound the host cannot check is refused, not assumed.
func freeBytes(dir string) (int64, error) {
	return 0, fmt.Errorf("count the free space under %s: %w", dir, errors.ErrUnsupported)
}
