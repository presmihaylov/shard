//go:build darwin && cgo

package vz

import "runtime"

// HostCPUs is what --cpus 0 gives a VM on this Mac: every host CPU, held inside the framework's ceiling.
func HostCPUs() uint {
	return DefaultCPUs(runtime.NumCPU(), cpuRange())
}
