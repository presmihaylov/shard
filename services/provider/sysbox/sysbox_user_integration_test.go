//go:build integration

package sysbox_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/services/provider/sysbox"
)

// waitGrace bounds a Wait on an entrypoint that ends by itself, so a supervisor that cannot drop never hangs the run.
const waitGrace = 30 * time.Second

// TestAUserSandboxRunsInTheOwnedNamespace is SHARD-211: in the userns the daemon pins, a holder born with setgroups denied made every --user drop and su die with EPERM.
func TestAUserSandboxRunsInTheOwnedNamespace(t *testing.T) {
	h := newHarness(t)

	spec := h.newSpec(t, "/bin/sh", "-c", "id -u; id -g; cat /proc/self/setgroups; exit 3")
	spec.User = "1000:1000"
	spec.Network = ownedNetwork(t, spec.ID)

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), waitGrace)
	defer cancel()

	// The exit code is the assertion: before the fix shard-init never got to exec, and the run ended with EPERM.
	exit, err := h.provider.Wait(ctx, spec.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Code != 3 {
		t.Errorf("the entrypoint ended %+v, want the exit code 3 it asked for", exit)
	}

	path, err := h.provider.LogPath(spec.ID)
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}
	if got := readFile(t, path); !strings.Contains(got, "1000\n1000\nallow") {
		t.Errorf("the entrypoint reported %q, want uid 1000, gid 1000 and setgroups allowed", got)
	}
}

// ownedNetwork pins the netns and the owning userns the way the daemon does for Sysbox, with no address on it.
func ownedNetwork(t *testing.T, id string) models.NetworkSpec {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("an owned namespace needs root")
	}

	m, err := netns.New()
	if err != nil {
		t.Skipf("no netns manager: %v", err)
	}

	if err := m.AddOwnedNamespace(t.Context(), id, sysbox.Userns); err != nil {
		t.Fatalf("AddOwnedNamespace: %v", err)
	}
	t.Cleanup(func() {
		if err := m.DeleteNamespace(context.Background(), id); err != nil {
			t.Errorf("DeleteNamespace: %v", err)
		}
	})

	return models.NetworkSpec{
		NetnsPath: netns.NamespacePath(id),
		Userns:    models.UserNamespace{Path: netns.UsernsPath(id), HostID: sysbox.Userns.HostID, Size: sysbox.Userns.Size},
	}
}
