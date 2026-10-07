//go:build integration

package runc_test

import (
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/conformance"
)

// TestDockersProfileKeepsTheGuestOffTheKeyring is SHARD-367: the kernel keyring is not namespaced, so the guest gets Docker's EPERM on every keyring call.
func TestDockersProfileKeepsTheGuestOffTheKeyring(t *testing.T) {
	h := newHarness(t)

	spec := h.newSpec(t)
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, err := bundle.Open(spec.StateDir)
	if err != nil {
		t.Fatalf("open the bundle: %v", err)
	}
	conformance.InstallKeyProbe(t, b.RootFS)
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, log := h.runToEnd(t, spec.ID, models.ProcessSpec{Name: "keyprobe", Argv: []string{conformance.KeyProbePath}})
	for _, want := range []string{"add_key: EPERM", "keyctl: EPERM", "request_key: EPERM"} {
		if !strings.Contains(log, want) {
			t.Errorf("the probe reported %q, want %q", log, want)
		}
	}
}
