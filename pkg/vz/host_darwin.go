//go:build darwin

package vz

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// HostCPUs is what --cpus 0 gives a VM on this Mac: every host CPU, held inside the framework's ceiling.
func HostCPUs() uint {
	return DefaultCPUs(runtime.NumCPU(), cpuRange())
}

// HostSaveRestore probes this Mac: the framework saves and restores a VM on Apple silicon from macOS 14.
func HostSaveRestore() bool {
	version, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return false
	}

	return SaveRestoreSupported(runtime.GOOS, runtime.GOARCH, version)
}
