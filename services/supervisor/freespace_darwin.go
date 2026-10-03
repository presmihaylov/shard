//go:build darwin

package supervisor

import (
	"fmt"
	"os"
)

// freePastEnd frees the space past the end of a log: APFS keeps blocks past the end of a slow append, and only a change of size frees them all (SHARD-420).
func freePastEnd(f *os.File, size int64) error {
	if err := f.Truncate(size + 1); err != nil {
		return fmt.Errorf("grow the log by a byte: %w", err)
	}

	return f.Truncate(size)
}
