package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The ambient set is how a capability survives the drop to another user: the kernel clears the
// permitted and the effective set when every id moves away from root.
// A tty makes the child a session leader with fd 0 as its controlling terminal, so job control works.
func sysProcAttr(credential *syscall.Credential, ambient []uintptr, tty bool, bound *os.File) *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Credential: credential, AmbientCaps: ambient, Setsid: tty, Setctty: tty, Ctty: 0}
	// The kernel starts the child in the bound, so no guest process ever runs in PID 1's cgroup, not even before its exec.
	if bound != nil {
		attr.UseCgroupFD, attr.CgroupFD = true, int(bound.Fd())
	}

	return attr
}

// setUndumpable clears the dumpable flag, so the kernel makes /proc/1/fd root-owned and unreadable to
// the guest. That is what keeps shard-init's exit channel on fd 0 out of the guest's reach on gVisor,
// where the guest has no CAP_SYS_PTRACE to override it.
func setUndumpable() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("clear the dumpable flag: %w", err)
	}

	return nil
}

// dropCapabilities empties every set on every thread, since capset binds one thread and a file op may run on any.
func dropCapabilities() error {
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&header, &data[0]); err != nil {
		return fmt.Errorf("read the capabilities of a files op as a non-root user: %w", err)
	}
	// An empty permitted set holds nothing to leak, and a cgo build, as the test binary is, refuses AllThreadsSyscall.
	if data[0].Permitted == 0 && data[1].Permitted == 0 {
		return nil
	}

	data = [2]unix.CapUserData{}
	_, _, errno := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data[0])), 0) //nolint:gosec // capset takes two struct pointers
	if errno != 0 {
		return fmt.Errorf("drop the capabilities of a files op as a non-root user: %w", errno)
	}

	return nil
}
