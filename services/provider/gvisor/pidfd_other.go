//go:build !linux

package gvisor

import "fmt"

// pidfdKill has no pidfd to pin a process by off Linux, where runsc does not run either.
func pidfdKill(pid int, _ func() (bool, error)) error {
	return fmt.Errorf("kill process %d: a pidfd needs linux, and %s runs on linux only", pid, Name)
}
