//go:build !darwin || !cgo

package vz

import "runtime"

// HostCPUs is the host count where no framework is linked to set a ceiling.
func HostCPUs() uint { return uint(max(runtime.NumCPU(), 0)) } //nolint:gosec // a cpu count fits
