//go:build !linux

package firecracker

import (
	"fmt"
	"runtime"
)

// CheckChrootBase refuses off Linux, where no jailer runs.
func CheckChrootBase(dir string) error {
	return fmt.Errorf("the jailer runs on Linux only, not %s, so %s holds no chroot", runtime.GOOS, dir)
}
