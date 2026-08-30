//go:build integration

package gvisor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// One stopped sandbox clones into two live ones at once, each running the entrypoint from the
// beginning over its own copy of the files, and the source stays stopped with its layer as it was.
func TestOneStoppedSandboxClonesIntoTwoIndependentSandboxes(t *testing.T) {
	h := newNetworkedHarness(t)
	source := h.start(t, "/bin/sh", "-c", "echo booted; while true; do sleep 0.2; done")

	execIn(t, h, source.ID, "echo from-the-source > /root/marker")

	if err := h.provider.Stop(t.Context(), source.ID, stopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	clones := []models.SandboxSpec{h.newSpec(t), h.newSpec(t)}

	var wg sync.WaitGroup
	errs := make([]error, len(clones))
	for i, clone := range clones {
		wg.Go(func() {
			started := time.Now()
			errs[i] = h.provider.Clone(t.Context(), source.ID, clone)
			t.Logf("clone into %s took %s", clone.ID, time.Since(started))
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Clone into %s: %v", clones[i].ID, err)
		}
	}

	for _, clone := range clones {
		assertAlive(t, h, clone.ID, true)
		assertMounted(t, h, clone.ID, true)

		if got := execIn(t, h, clone.ID, "cat /root/marker"); !strings.Contains(got, "from-the-source") {
			t.Errorf("clone %s has %q in /root/marker, want what the source wrote before the stop", clone.ID, got)
		}
		if got := execIn(t, h, clone.ID, "hostname; ip -4 -o addr show dev eth0"); !strings.Contains(got, clone.ID) || !strings.Contains(got, clone.Network.Address.Addr().String()) {
			t.Errorf("clone %s reports %q, want its own hostname and address", clone.ID, got)
		}
		if got := execIn(t, h, clone.ID, "nc -w 5 1.1.1.1 80 < /dev/null && echo reached"); !strings.Contains(got, "reached") {
			t.Errorf("clone %s could not reach the internet: %s", clone.ID, got)
		}

		// The entrypoint ran from the beginning, in the clone's own log, and the source's exit is not its own.
		path, err := h.provider.LogPath(clone.ID)
		if err != nil {
			t.Fatalf("LogPath: %v", err)
		}
		if booted := strings.Count(readFile(t, path), "booted"); booted != 1 {
			t.Errorf("clone %s printed the banner %d times, want once from a fresh run", clone.ID, booted)
		}
		if _, err := os.Stat(filepath.Join(stateDirOf(t, h, clone.ID), "shard", "exit.json")); err == nil {
			t.Errorf("clone %s carries the source's exit status", clone.ID)
		}
	}

	// Each clone writes over its own copy of the layer, so neither the other clone nor the source sees it.
	execIn(t, h, clones[0].ID, "echo from-clone-0 > /root/marker")
	if got := execIn(t, h, clones[1].ID, "cat /root/marker"); strings.Contains(got, "from-clone-0") {
		t.Errorf("clone %s sees what clone %s wrote: %q", clones[1].ID, clones[0].ID, got)
	}

	assertAlive(t, h, source.ID, false)
	if got := readFile(t, filepath.Join(stateDirOf(t, h, source.ID), "overlay", "upper", "root", "marker")); got != "from-the-source\n" {
		t.Errorf("the source's layer holds %q after the clones, want its own write", got)
	}

	// The source still starts again over what it kept, so the clones consumed nothing.
	if err := h.provider.Start(t.Context(), source.ID); err != nil {
		t.Fatalf("Start of the source after two clones: %v", err)
	}
	if got := execIn(t, h, source.ID, "cat /root/marker"); !strings.Contains(got, "from-the-source") {
		t.Errorf("the source has %q in /root/marker after the start, want its own write", got)
	}
}

func TestCloneRefusesARunningSourceAndALiveId(t *testing.T) {
	h := newHarness(t)
	source := h.start(t, "/bin/sh", "-c", "while true; do sleep 1; done")

	err := h.provider.Clone(t.Context(), source.ID, h.newSpec(t))
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("Clone of a running source returned %v, want a refusal that names the stop", err)
	}
	assertAlive(t, h, source.ID, true)

	ctx := context.Background()
	if err := h.provider.Stop(ctx, source.ID, stopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	live := h.start(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	err = h.provider.Clone(ctx, source.ID, live)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Clone over a live id returned %v, want a refusal", err)
	}
	assertAlive(t, h, live.ID, true)
	assertMounted(t, h, live.ID, true)
}

func stateDirOf(t *testing.T, h *harness, id string) string {
	t.Helper()

	dir, err := h.stateDir(id)
	if err != nil {
		t.Fatal(err)
	}

	return dir
}
