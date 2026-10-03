//go:build !linux && !darwin

package vz

import "errors"

func startOf(int) (int64, error) {
	return 0, errors.New("vz: a process start time is read on linux and darwin only")
}

func scan(string, func([]string) bool) (Process, error) {
	return Process{}, errors.New("vz: the process table is read on linux and darwin only")
}
