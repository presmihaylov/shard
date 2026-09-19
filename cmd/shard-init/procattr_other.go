//go:build !linux

package main

import "syscall"

// Only Linux carries ambient capabilities, and only Linux ever runs a sandbox. This keeps the
// package building on a developer machine, where the tests never drop to another user.
func sysProcAttr(credential *syscall.Credential, _ []uintptr, tty bool) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Credential: credential, Setsid: tty, Setctty: tty, Ctty: 0}
}

// setUndumpable is a no-op off Linux, where no sandbox runs and the tests never fork a guest.
func setUndumpable() error { return nil }
