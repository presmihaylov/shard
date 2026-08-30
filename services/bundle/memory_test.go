package bundle_test

import (
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

// TestTheBoundIsTheNumberTheOperatorTyped pins what the guest reads as MemTotal: runsc gives the
// sentry this number, so any headroom in it would be a bound the operator never asked for.
func TestTheBoundIsTheNumberTheOperatorTyped(t *testing.T) {
	got := bundle.MemoryBound(models.Resources{MemoryMiB: 128})

	if got != 128*1024*1024 {
		t.Fatalf("MemoryBound = %d, want 134217728", got)
	}
}

func TestAnUnboundedSandboxGetsNoBound(t *testing.T) {
	for _, r := range []models.Resources{{}, {MemoryMiB: 0}} {
		if got := bundle.MemoryBound(r); got != 0 {
			t.Fatalf("MemoryBound(%v) = %d, want 0", r, got)
		}
	}
}
