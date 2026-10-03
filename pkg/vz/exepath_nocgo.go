//go:build darwin && !cgo

package vz

import "errors"

// executable refuses where libproc is not linked.
func executable(int) (string, error) {
	return "", errors.New("vz: the executable of a process is read through libproc, which needs cgo")
}
