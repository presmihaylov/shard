//go:build !linux

package xfs

func reserve(string, int64) error {
	return ErrNotLinux
}
