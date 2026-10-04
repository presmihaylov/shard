//go:build integration

package gvisor_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// Each sandbox needs a separate layer copy to keep its writes private.
func TestOneSnapshotSeedsTwoIndependentSandboxes(t *testing.T) {
	h := newNetworkedHarness(t)
	source := h.start(t, "/bin/sh", "-c", "while true; do sleep 0.2; done")

	execIn(t, h, source.ID, "echo from-the-source > /root/marker")

	if err := h.provider.Stop(t.Context(), source.ID, stopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	files := t.TempDir()
	if err := h.provider.Snapshot(t.Context(), source.ID, files); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	seeded := []models.SandboxSpec{h.newSpec(t), h.newSpec(t)}

	var wg sync.WaitGroup
	errs := make([]error, len(seeded))
	for i := range seeded {
		seeded[i].Seed = files
		wg.Go(func() {
			started := time.Now()
			if errs[i] = h.provider.Create(t.Context(), seeded[i]); errs[i] == nil {
				errs[i] = h.provider.Start(t.Context(), seeded[i].ID)
			}
			t.Logf("the create from the snapshot into %s took %s", seeded[i].ID, time.Since(started))
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %s from the snapshot: %v", seeded[i].ID, err)
		}
	}

	for _, spec := range seeded {
		assertAlive(t, h, spec.ID, true)
		assertMounted(t, h, spec.ID, true)

		if got := execIn(t, h, spec.ID, "cat /root/marker"); !strings.Contains(got, "from-the-source") {
			t.Errorf("sandbox %s has %q in /root/marker, want what the source wrote before the stop", spec.ID, got)
		}
		if got := execIn(t, h, spec.ID, "hostname; ip -4 -o addr show dev eth0"); !strings.Contains(got, spec.ID) || !strings.Contains(got, spec.Network.Address.Addr().String()) {
			t.Errorf("sandbox %s reports %q, want its own hostname and address", spec.ID, got)
		}
		if got := execIn(t, h, spec.ID, "nc -w 5 1.1.1.1 80 < /dev/null && echo reached"); !strings.Contains(got, "reached") {
			t.Errorf("sandbox %s could not reach the internet: %s", spec.ID, got)
		}
	}

	// Each sandbox writes over its own copy of the layer, so neither the other one nor the source sees it.
	execIn(t, h, seeded[0].ID, "echo from-the-first > /root/marker")
	if got := execIn(t, h, seeded[1].ID, "cat /root/marker"); strings.Contains(got, "from-the-first") {
		t.Errorf("sandbox %s sees what sandbox %s wrote: %q", seeded[1].ID, seeded[0].ID, got)
	}

	assertAlive(t, h, source.ID, false)

	// The upper lives in disk.img, so a restart plus exec is how the source's layer is read after the copy.
	if err := h.provider.Start(t.Context(), source.ID); err != nil {
		t.Fatalf("Start of the source after the snapshot: %v", err)
	}
	if got := execIn(t, h, source.ID, "cat /root/marker"); !strings.Contains(got, "from-the-source") {
		t.Errorf("the source has %q in /root/marker after the start, want its own write", got)
	}
}

func TestSnapshotRefusesARunningSource(t *testing.T) {
	h := newHarness(t)
	source := h.start(t, "/bin/sh", "-c", "while true; do sleep 1; done")

	err := h.provider.Snapshot(t.Context(), source.ID, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("Snapshot of a running source returned %v, want a refusal that names the stop", err)
	}
	assertAlive(t, h, source.ID, true)
}

func stateDirOf(t *testing.T, h *harness, id string) string {
	t.Helper()

	dir, err := h.stateDir(id)
	if err != nil {
		t.Fatal(err)
	}

	return dir
}
