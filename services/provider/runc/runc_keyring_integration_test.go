//go:build integration

package runc_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/conformance"
)

// waitGrace bounds a Wait on an entrypoint that ends by itself, so a probe that hangs never hangs the run.
const waitGrace = 30 * time.Second

// TestDockersProfileKeepsTheGuestOffTheKeyring is SHARD-367: the kernel keyring is not namespaced, so the guest gets Docker's EPERM on every keyring call.
func TestDockersProfileKeepsTheGuestOffTheKeyring(t *testing.T) {
	h := newHarness(t)

	spec := h.newSpec(t, conformance.KeyProbePath)
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
	for _, want := range []string{"add_key: EPERM", "keyctl: EPERM", "request_key: EPERM"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("the probe reported %q, want %q", log, want)
		}
	}
}
