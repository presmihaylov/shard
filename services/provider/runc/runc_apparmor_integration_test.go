//go:build integration

package runc_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

// TestTheGuestRunsUnderDockersAppArmorProfile is SHARD-391: where the module is on, no runc sandbox runs unconfined.
func TestTheGuestRunsUnderDockersAppArmorProfile(t *testing.T) {
	enabled, err := os.ReadFile("/sys/module/apparmor/parameters/enabled")
	if err != nil || !bytes.HasPrefix(enabled, []byte("Y")) {
		t.Skipf("the AppArmor module is off on this host (%q, %v)", enabled, err)
	}
	h := newHarness(t)

	spec := h.newSpec(t)
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// shard-init forks every process, so one reads the profile PID 1 runs under.
	_, log := h.runToEnd(t, spec.ID, models.ProcessSpec{Name: "attr", Argv: []string{"cat", "/proc/self/attr/current"}})
	if want := bundle.AppArmorProfile + " (enforce)"; !strings.Contains(log, want) {
		t.Errorf("the guest runs under %q, want %q", log, want)
	}
}
