package bundle

import (
	"fmt"
	"os"
)

// ClearRun drops what an earlier run left of the supervisor's files. The guest can leave a tree at ReadyFile, so a plain remove is not enough (SHARD-635).
func (b Bundle) ClearRun() error {
	for _, stale := range []string{b.ExitFile, b.ReadyFile, b.ChangedFile} {
		// The guest is down here, and RemoveAll follows no link, so nothing past the tree it left goes with it.
		if err := os.RemoveAll(stale); err != nil {
			return fmt.Errorf("clear %s: %w", stale, err)
		}
	}

	return nil
}
