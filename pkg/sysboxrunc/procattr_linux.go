//go:build linux

package sysboxrunc

import "syscall"

// execAttr makes sysbox-runc exec die with the daemon: the kernel sends it SIGKILL when its parent thread ends.
func execAttr(tty bool) *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if tty {
		// runc gives the guest a terminal only when its own stdio is one it controls.
		attr.Setsid, attr.Setctty, attr.Ctty = true, true, 0
	}

	return attr
}
