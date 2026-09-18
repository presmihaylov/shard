package bundle_test

import (
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

// TestAnUnboundedSandboxStillGetsThePidsDefault pins the one way pids differs from memory: a sandbox
// that names no bound is not unbounded, it takes DefaultPidsMax, so a fork bomb cannot exhaust host PIDs.
func TestAnUnboundedSandboxStillGetsThePidsDefault(t *testing.T) {
	for _, r := range []models.Resources{{}, {PidsMax: 0}} {
		if got := bundle.PidsBound(r); got != bundle.DefaultPidsMax {
			t.Errorf("PidsBound(%v) = %d, want the default %d", r, got, bundle.DefaultPidsMax)
		}
	}
}

func TestAPidsBoundIsTheNumberTheOperatorTyped(t *testing.T) {
	if got := bundle.PidsBound(models.Resources{PidsMax: 100}); got != 100 {
		t.Errorf("PidsBound = %d, want 100", got)
	}
}
