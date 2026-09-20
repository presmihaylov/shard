//go:build !darwin

package vz

// HostSaveRestore is false off a Mac: there is no framework to save with.
func HostSaveRestore() bool { return false }
