//go:build !linux

package bundle

import "errors"

// errNoOverlay keeps a developer Mac honest: Build works there, running a sandbox does not. It is a
// plain error, because a missing kernel feature is not a provider refusing a verb.
var errNoOverlay = errors.New("the writable layer needs overlayfs, which only Linux has")

func (b Bundle) Mount(lower string) error { return errNoOverlay }

func (b Bundle) Unmount() error { return errNoOverlay }

func (b Bundle) Mounted() (bool, error) { return false, errNoOverlay }
