package gvisor_test

import (
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/gvisor"
)

// TestTheThrottleSitsBelowTheCeiling is the whole point of SHARD-97: a guest that reaches its bound
// meets reclaim, which slows it, and never the OOM killer, which would end the sandbox.
func TestTheThrottleSitsBelowTheCeiling(t *testing.T) {
	r := models.Resources{MemoryMiB: 128}

	bound, high, ceiling := bundle.MemoryBound(r), gvisor.MemoryThrottle(r), gvisor.MemoryCeiling(r)

	if bound >= high || high >= ceiling {
		t.Fatalf("the bound is %d, the throttle %d and the ceiling %d, which is not the order that keeps a guest at its bound alive", bound, high, ceiling)
	}
}

// TestTheThrottleClearsTheBound proves the guest is not throttled before it reaches what it was
// given: the host cgroup charges the sentry's own working set to the same cgroup.
func TestTheThrottleClearsTheBound(t *testing.T) {
	const mib = 128

	high := gvisor.MemoryThrottle(models.Resources{MemoryMiB: mib})

	if high <= mib*1024*1024 {
		t.Fatalf("the throttle is %d, which is at or below the %d MiB bound itself", high, mib)
	}
}

func TestAnUnboundedSandboxGetsNeitherKnob(t *testing.T) {
	for _, r := range []models.Resources{{}, {MemoryMiB: 0}} {
		if got := gvisor.MemoryThrottle(r); got != 0 {
			t.Fatalf("MemoryThrottle(%v) = %d, want 0", r, got)
		}
		if got := gvisor.MemoryCeiling(r); got != 0 {
			t.Fatalf("MemoryCeiling(%v) = %d, want 0", r, got)
		}
	}
}
