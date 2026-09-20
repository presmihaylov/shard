//go:build darwin

package vz

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// HostSaveRestore probes this Mac: the framework saves and restores a VM on Apple silicon from macOS 14.
func HostSaveRestore() bool {
	version, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return false
	}

	return SaveRestoreSupported(runtime.GOOS, runtime.GOARCH, version)
}
