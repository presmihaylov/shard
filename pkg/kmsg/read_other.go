//go:build !linux

package kmsg

// Read returns nothing off Linux: there is no kernel ring buffer to read.
func Read() ([]Record, error) { return nil, ErrNotSupported }
