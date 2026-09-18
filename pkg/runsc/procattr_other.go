//go:build !linux

package runsc

import "syscall"

// execAttr has no parent-death signal off Linux, where only the unit tests run an exec.
func execAttr(tty bool) *syscall.SysProcAttr {
	if !tty {
		return nil
	}

	return &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
}
