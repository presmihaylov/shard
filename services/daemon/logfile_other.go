//go:build !darwin

package daemon

import "errors"

func openLog(string) error {
	return errors.New("--log is for the daemon on a Mac; on Linux the journal keeps the daemon's log")
}
