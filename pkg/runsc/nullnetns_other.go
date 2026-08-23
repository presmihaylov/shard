//go:build !linux

package runsc

import "errors"

// DropNullNetns needs a Linux unmount. New already refuses off Linux, so nothing reaches this.
func (r *Runner) DropNullNetns() error {
	return errors.New("dropping the runsc null-netns mount needs Linux")
}
