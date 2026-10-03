//go:build !linux

package firecracker_test

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// stopped is whether ps reports pid stopped; Darwin stops a whole process at once.
func stopped(pid int) (bool, error) {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false, fmt.Errorf("ps %d: %w", pid, err)
	}

	return strings.HasPrefix(strings.TrimSpace(string(out)), "T"), nil
}
