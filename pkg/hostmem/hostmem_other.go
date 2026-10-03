//go:build !linux && !darwin

package hostmem

import (
	"fmt"
	"runtime"
)

func Total() (int64, error) {
	return 0, fmt.Errorf("read the host memory: %s is not supported", runtime.GOOS)
}
