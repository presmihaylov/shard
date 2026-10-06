//go:build !darwin

package firecracker

// exiting is false off darwin: Linux refuses or ends a dial that races the close of a listener, so the peer's state adds nothing.
func exiting(int) (bool, error) { return false, nil }
