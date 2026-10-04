//go:build !darwin

package pgroup

// Elsewhere an exiting or zombie member still takes the signal, so EPERM means a live member this process may not signal.
func noneLive(int) (bool, error) { return false, nil }
