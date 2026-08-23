package main

import "syscall"

// The ambient set is how a capability survives the drop to another user: the kernel clears the
// permitted and the effective set when every id moves away from root.
func sysProcAttr(credential *syscall.Credential, ambient []uintptr) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Credential: credential, AmbientCaps: ambient}
}
