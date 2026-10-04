//go:build !darwin

package cli

// Only a Mac daemon carries a VM shim, so there is no state to report elsewhere.
func shimState() string { return "" }

func shimLine() string { return "" }
