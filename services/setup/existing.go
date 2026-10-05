package setup

import (
	"context"
	"errors"
)

// Installation is what setup finds on a host it set up before, or a manual one it does not own.
type Installation struct {
	Version  string
	Provider string
	Service  string
	Manual   bool
}

// Detect finds an existing installation from the manifest, the binaries and the services (shard-worker2).
func Detect(context.Context, Host) (Installation, bool, error) {
	return Installation{}, false, nil
}

// existing is §11: check or repair, upgrade, uninstall (shard-worker2).
func (s *Setup) existing(context.Context, Installation) error {
	return errors.New("the existing installation menu is not built yet")
}
