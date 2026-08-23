//go:build linux

package runsc

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"syscall"
)

// nullNetns is the nsfs runsc bind mounts into its own root on the first create.
const nullNetns = "null-netns"

// DropNullNetns unmounts what runsc keeps for itself. No runsc verb drops it, so it outlives every
// sandbox and blocks an rm -rf of the root that holds it. Dropping it beside a live sandbox is
// safe: the sandbox holds its own namespace, and the next create binds this one again.
func (r *Runner) DropNullNetns() error {
	path := filepath.Join(r.root, nullNetns)

	err := syscall.Unmount(path, 0)
	// EINVAL is a path that carries no mount, which is a root no create has run under yet.
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("unmount %s: %w", path, err)
	}

	return nil
}
