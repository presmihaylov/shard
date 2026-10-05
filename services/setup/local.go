package setup

import (
	"context"
	"errors"
)

// Local is what a local setup installs: one provider, and whether a service starts it at boot.
type Local struct {
	Provider    string
	StartAtBoot bool
}

// local is §5 to §10: the provider, automatic startup, preflight, review, and apply (shard-worker8).
func (s *Setup) local(context.Context) error {
	return errors.New("local setup is not built yet")
}
