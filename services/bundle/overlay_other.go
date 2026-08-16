//go:build !linux

package bundle

import (
	"fmt"

	"github.com/presmihaylov/shard/models"
)

// errNoOverlay keeps a developer Mac honest: Build works there, running a sandbox does not.
var errNoOverlay = fmt.Errorf("%w: the writable layer needs overlayfs, which only Linux has", models.ErrUnsupported)

func Mount(b Bundle) error { return errNoOverlay }

func Unmount(b Bundle) error { return errNoOverlay }

func Mounted(b Bundle) (bool, error) { return false, errNoOverlay }
