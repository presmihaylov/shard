//go:build !linux && !darwin

package vz

import "errors"

func startOf(int) (int64, error) {
	return 0, errors.New("vz: a process start time is read on linux and darwin only")
}
