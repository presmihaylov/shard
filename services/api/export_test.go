package api

import (
	"testing"
	"time"
)

// SetClock replaces the clock an idle body's deadline counts from, for one test.
func SetClock(tb testing.TB, clock func() time.Time) {
	old := now
	now = clock
	tb.Cleanup(func() { now = old })
}
