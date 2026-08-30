package cli

import (
	"fmt"

	"github.com/presmihaylov/shard/models"
)

// requireVerb refuses an optional verb the provider does not claim, before anything is held or claimed.
func requireVerb(provider models.Provider, verb string) error {
	caps := provider.Capabilities()

	var claimed bool
	switch verb {
	case models.VerbPause:
		claimed = caps.Pause
	case models.VerbResume:
		claimed = caps.Resume
	case models.VerbFork:
		claimed = caps.Fork
	default:
		return fmt.Errorf("requireVerb: %q is not an optional verb", verb)
	}

	if claimed {
		return nil
	}

	return models.Unsupported(provider.Name(), verb)
}
