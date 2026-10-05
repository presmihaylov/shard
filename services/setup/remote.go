package setup

import (
	"context"
	"errors"
)

// Remote is a connection to a shard serve front. APIKey never reaches a log, an error or a summary.
type Remote struct {
	URL    string
	APIKey string
	Save   bool
}

// remote is §12 and §13: the URL, the key, the verify checklist and the save (shard-worker5).
func (s *Setup) remote(context.Context) error {
	return errors.New("remote setup is not built yet")
}
