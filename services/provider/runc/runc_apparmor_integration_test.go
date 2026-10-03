//go:build integration

package runc_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/bundle"
)

// TestTheGuestRunsUnderDockersAppArmorProfile is SHARD-391: where the module is on, no runc sandbox runs unconfined.
func TestTheGuestRunsUnderDockersAppArmorProfile(t *testing.T) {
	enabled, err := os.ReadFile("/sys/module/apparmor/parameters/enabled")
	if err != nil || !bytes.HasPrefix(enabled, []byte("Y")) {
		t.Skipf("the AppArmor module is off on this host (%q, %v)", enabled, err)
	}
	h := newHarness(t)

	spec := h.newSpec(t, "cat", "/proc/self/attr/current")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), waitGrace)
	defer cancel()
	if _, err := h.provider.Wait(ctx, spec.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	path, err := h.provider.LogPath(spec.ID)
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}
	log, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	if want := bundle.AppArmorProfile + " (enforce)"; !strings.Contains(string(log), want) {
		t.Errorf("the guest runs under %q, want %q", log, want)
	}
}
