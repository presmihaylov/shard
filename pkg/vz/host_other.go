//go:build !darwin

package vz

import "runtime"

// HostCPUs is the host count off a Mac, where no framework sets a ceiling.
func HostCPUs() uint { return uint(max(runtime.NumCPU(), 0)) } //nolint:gosec // a cpu count fits

// HostSaveRestore is false off a Mac: there is no framework to save with.
func HostSaveRestore() bool { return false }
