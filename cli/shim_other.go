//go:build !darwin

package cli

// Only a Mac daemon carries a VM shim, so there is no line to print elsewhere.
func shimLine() string { return "" }
