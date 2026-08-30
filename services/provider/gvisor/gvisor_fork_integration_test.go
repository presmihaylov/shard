//go:build integration

package gvisor_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// One snapshot restores into two live sandboxes at once, each with its own id, namespace and address,
// and neither sees the other's writes. The source stays paused and resumes after both are up.
func TestOneSnapshotForksIntoTwoIndependentSandboxes(t *testing.T) {
	h := newNetworkedHarness(t)
	source := h.start(t, "/bin/sh", "-c", "i=0; while true; do i=$((i+1)); echo tick $i; sleep 0.2; done")
	snapshot := filepath.Join(t.TempDir(), "snapshot")

	execIn(t, h, source.ID, "echo from-the-source > /root/marker")

	if err := h.provider.Pause(t.Context(), source.ID, snapshot); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	forks := []models.SandboxSpec{h.newSpec(t), h.newSpec(t)}

	var wg sync.WaitGroup
	errs := make([]error, len(forks))
	for i, fork := range forks {
		wg.Go(func() {
			started := time.Now()
			errs[i] = h.provider.Fork(t.Context(), snapshot, fork)
			t.Logf("fork into %s took %s", fork.ID, time.Since(started))
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Fork into %s: %v", forks[i].ID, err)
		}
	}

	if forks[0].Network.Address == forks[1].Network.Address {
		t.Fatalf("both forks got %s", forks[0].Network.Address)
	}

	for _, fork := range forks {
		assertAlive(t, h, fork.ID, true)

		if err := h.net.Reapply(t.Context(), fork.ID); err != nil {
			t.Fatalf("Reapply for %s: %v", fork.ID, err)
		}

		if got := execIn(t, h, fork.ID, "cat /root/marker"); !strings.Contains(got, "from-the-source") {
			t.Errorf("fork %s has %q in /root/marker, want what the source wrote before the pause", fork.ID, got)
		}
		if got := execIn(t, h, fork.ID, "hostname; ip -4 -o addr show dev eth0"); !strings.Contains(got, fork.ID) || !strings.Contains(got, fork.Network.Address.Addr().String()) {
			t.Errorf("fork %s reports %q, want its own hostname and address", fork.ID, got)
		}
		if got := execIn(t, h, fork.ID, "nc -w 5 1.1.1.1 80 < /dev/null && echo reached"); !strings.Contains(got, "reached") {
			t.Errorf("fork %s could not reach the internet: %s", fork.ID, got)
		}
	}

	// Each fork writes over its own copy of the layer, so the other one never sees the write.
	execIn(t, h, forks[0].ID, "echo from-fork-0 > /root/marker")
	if got := execIn(t, h, forks[1].ID, "cat /root/marker"); strings.Contains(got, "from-fork-0") {
		t.Errorf("fork %s sees what fork %s wrote: %q", forks[1].ID, forks[0].ID, got)
	}

	// The memory image restored twice, so both loops count on from the same tick, in their own logs.
	time.Sleep(time.Second)
	for _, fork := range forks {
		path, err := h.provider.LogPath(fork.ID)
		if err != nil {
			t.Fatalf("LogPath: %v", err)
		}
		if ticks := strings.Count(readFile(t, path), "tick"); ticks == 0 {
			t.Errorf("fork %s wrote no ticks to its own log", fork.ID)
		}
	}

	// The source is read and nothing more: it is still paused and it still resumes.
	assertAlive(t, h, source.ID, false)
	if _, err := os.Stat(filepath.Join(snapshot, "checkpoint.img")); err != nil {
		t.Errorf("the forks consumed the snapshot: %v", err)
	}
	if _, err := h.net.Allocate(t.Context(), source.ID); err != nil {
		t.Fatalf("Allocate for the source again: %v", err)
	}
	if err := h.provider.Resume(t.Context(), source.ID, snapshot); err != nil {
		t.Fatalf("Resume of the source after two forks: %v", err)
	}
	assertAlive(t, h, source.ID, true)
	if got := execIn(t, h, source.ID, "cat /root/marker"); !strings.Contains(got, "from-the-source") {
		t.Errorf("the source has %q in /root/marker after the forks, want its own write", got)
	}
}

// A fork of a running source reads the snapshot its last pause wrote, and the source runs on untouched.
func TestAForkOfARunningSourceReadsItsLastSnapshot(t *testing.T) {
	h := newNetworkedHarness(t)
	source := h.start(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	snapshot := filepath.Join(t.TempDir(), "snapshot")

	if err := h.provider.Pause(t.Context(), source.ID, snapshot); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if _, err := h.net.Allocate(t.Context(), source.ID); err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := h.provider.Resume(t.Context(), source.ID, snapshot); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	execIn(t, h, source.ID, "echo after-the-pause > /root/later")

	fork := h.newSpec(t)
	if err := h.provider.Fork(t.Context(), snapshot, fork); err != nil {
		t.Fatalf("Fork: %v", err)
	}

	if got := execIn(t, h, fork.ID, "ls /root"); strings.Contains(got, "later") {
		t.Errorf("the fork has %q in /root, want the files as they were at the pause", got)
	}
	assertAlive(t, h, source.ID, true)
}

func TestForkRefusesAnIdThatIsLive(t *testing.T) {
	h := newHarness(t)
	source := h.start(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	snapshot := filepath.Join(t.TempDir(), "snapshot")

	if err := h.provider.Pause(t.Context(), source.ID, snapshot); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	live := h.start(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	err := h.provider.Fork(t.Context(), snapshot, live)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Fork over a live id returned %v, want a refusal", err)
	}
	assertAlive(t, h, live.ID, true)
	assertMounted(t, h, live.ID, true)

	ctx := context.Background()
	if err := h.provider.Stop(ctx, live.ID, stopGrace); err != nil {
		t.Fatalf("Stop after the refusal: %v", err)
	}
	if got := fmt.Sprint(h.provider.Remove(ctx, live.ID)); got != "<nil>" {
		t.Errorf("Remove after the refusal: %s", got)
	}
}
