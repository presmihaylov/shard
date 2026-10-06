//go:build !linux

package termrelay

import "errors"

// Relay is the terminal mode of the guest supervisor, which only a Linux guest runs.
func Relay([]string) (int, error) {
	return 0, errors.New("the terminal relay runs in a Linux guest only")
}
