//go:build !linux

package main

import "syscall"

// Only Linux carries ambient capabilities, and only Linux ever runs a sandbox. This keeps the
// package building on a developer machine, where the tests never drop to another user.
func sysProcAttr(credential *syscall.Credential, _ []uintptr) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Credential: credential}
}
