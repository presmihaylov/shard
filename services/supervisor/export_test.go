package supervisor

import (
	"testing"
	"time"
)

// SetStartTimeout shortens the wait for an exec's first frame, which a test cannot sit out at its full length.
func SetStartTimeout(tb testing.TB, d time.Duration) {
	old := startTimeout
	startTimeout = d
	tb.Cleanup(func() { startTimeout = old })
}
