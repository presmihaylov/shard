//go:build !linux

package launch

// Shim is the launch mode of the guest supervisor, which only a Linux guest runs.
func Shim([]string) error { return errUnsupported }
