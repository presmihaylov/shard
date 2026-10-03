//go:build !linux

package memfd

import "os"

func create(string, int64) (*os.File, error) {
	return nil, ErrNotLinux
}

// fixed is false off Linux, where no file carries a seal.
func fixed(*os.File) (bool, error) {
	return false, nil
}
