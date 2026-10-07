//go:build integration

package sysbox_test

import (
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/conformance"
)

// TestTheGuestCannotSpendTheKeyringQuota is SHARD-367: every guest root is the same host uid, so one guest that filled its key quota failed every later create.
func TestTheGuestCannotSpendTheKeyringQuota(t *testing.T) {
	h := newHarness(t)

	spec := h.newSpec(t)
	spec.Network = ownedNetwork(t, spec.ID)

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

	_, got := h.runToEnd(t, spec.ID, models.ProcessSpec{Name: "keyprobe", Argv: []string{conformance.KeyProbePath}})
	for _, want := range []string{"add_key: ENOSYS", "keyctl: ENOSYS", "request_key: ENOSYS"} {
		if !strings.Contains(got, want) {
			t.Errorf("the probe reported %q, want %q", got, want)
		}
	}
}
