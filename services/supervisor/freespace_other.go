//go:build !darwin

package supervisor

import "os"

// freePastEnd frees the space past the end of a log: XFS keeps speculative preallocation there, which du counts, and a truncate to its own size frees it.
func freePastEnd(f *os.File, size int64) error {
	return f.Truncate(size)
}
