//go:build !linux

package xfs

func reserve(string, int64) error {
	return ErrNotLinux
}

func room(string) (int64, error) {
	return 0, ErrNotLinux
}
