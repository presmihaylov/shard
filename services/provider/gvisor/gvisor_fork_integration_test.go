//go:build integration

package gvisor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// A running source forks twice and runs on as the same sentry; each fork has its own id, namespace
// and address, and no write crosses between the source and either fork (SHARD-457).
func TestALiveForkLeavesTheSourceRunningAndSharesNothing(t *testing.T) {
	h := newNetworkedHarness(t)
	source := h.start(t, "/bin/sh", "-c", "i=0; while true; do i=$((i+1)); echo tick $i; sleep 0.2; done")
	execIn(t, h, source.ID, "echo from-the-source > /root/marker")
	sentry := h.sentryPID(t, source.ID)

	// The service holds the source's lock across a fork, so two forks of one source run one after the other.
	forks := []models.SandboxSpec{h.newSpec(t), h.newSpec(t)}
	for _, fork := range forks {
		started := time.Now()
		if err := h.provider.Fork(t.Context(), source.ID, fork); err != nil {
			t.Fatalf("Fork into %s: %v", fork.ID, err)
		}
		t.Logf("fork into %s took %s", fork.ID, time.Since(started))
	}

	if forks[0].Network.Address == forks[1].Network.Address {
		t.Fatalf("both forks got %s", forks[0].Network.Address)
	}
	if got := h.sentryPID(t, source.ID); got != sentry {
		t.Fatalf("the source's sentry is pid %d after the forks, want the same %d", got, sentry)
	}
	assertRunsUnmarked(t, h, source.ID)

	for _, fork := range forks {
		assertAlive(t, h, fork.ID, true)

		if err := h.net.Reapply(t.Context(), fork.ID); err != nil {
			t.Fatalf("Reapply for %s: %v", fork.ID, err)
		}

		if got := execIn(t, h, fork.ID, "cat /root/marker"); !strings.Contains(got, "from-the-source") {
			t.Errorf("fork %s has %q in /root/marker, want what the source wrote before the fork", fork.ID, got)
		}
		if got := execIn(t, h, fork.ID, "hostname; ip -4 -o addr show dev eth0"); !strings.Contains(got, fork.ID) || !strings.Contains(got, fork.Network.Address.Addr().String()) {
			t.Errorf("fork %s reports %q, want its own hostname and address", fork.ID, got)
		}
		if got := execIn(t, h, fork.ID, "nc -w 5 1.1.1.1 80 < /dev/null && echo reached"); !strings.Contains(got, "reached") {
			t.Errorf("fork %s could not reach the internet: %s", fork.ID, got)
		}
	}
	if got := execIn(t, h, source.ID, "nc -w 5 1.1.1.1 80 < /dev/null && echo reached"); !strings.Contains(got, "reached") {
		t.Errorf("the source could not reach the internet after its forks: %s", got)
	}

	// Each fork writes over its own copy of the layer, so neither the other fork nor the source sees the write.
	execIn(t, h, forks[0].ID, "echo from-fork-0 > /root/marker")
	execIn(t, h, source.ID, "echo after-the-forks > /root/later")
	if got := execIn(t, h, forks[1].ID, "cat /root/marker; ls /root"); strings.Contains(got, "from-fork-0") || strings.Contains(got, "later") {
		t.Errorf("fork %s sees a write of fork %s or of the source: %q", forks[1].ID, forks[0].ID, got)
	}
	if got := execIn(t, h, source.ID, "cat /root/marker"); !strings.Contains(got, "from-the-source") {
		t.Errorf("the source has %q in /root/marker after a fork wrote its own, want its own write", got)
	}

	// The memory restored twice, so both loops count on in their own logs.
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
}

// A fork captures the source as it runs now, so a write made after an earlier pause and resume is in it (SHARD-457).
func TestAForkCapturesTheSourceAsItRunsNotItsLastCheckpoint(t *testing.T) {
	h := newNetworkedHarness(t)
	source := h.start(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	checkpoint := filepath.Join(t.TempDir(), "checkpoint")

	if err := h.provider.Pause(t.Context(), source.ID, checkpoint); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if _, err := h.net.Allocate(t.Context(), source.ID); err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := h.provider.Resume(t.Context(), source.ID, checkpoint); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	execIn(t, h, source.ID, "echo after-the-pause > /root/later")

	fork := h.newSpec(t)
	if err := h.provider.Fork(t.Context(), source.ID, fork); err != nil {
		t.Fatalf("Fork: %v", err)
	}

	if got := execIn(t, h, fork.ID, "cat /root/later"); !strings.Contains(got, "after-the-pause") {
		t.Errorf("the fork has %q in /root/later, want the write the source made after its last checkpoint", got)
	}
	assertRunsUnmarked(t, h, source.ID)
}

// A daemon cut inside a live fork leaves the source frozen with its mark, and the next daemon's read of it thaws it (SHARD-457).
func TestTheNextDaemonThawsASourceACutForkLeftFrozen(t *testing.T) {
	h := newHarness(t)
	source := h.start(t, "/bin/sh", "-c", "while true; do sleep 1; done")

	if err := os.WriteFile(filepath.Join(stateDirOf(t, h, source.ID), "fork-frozen"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runsc(t, "pause", source.ID).CombinedOutput(); err != nil {
		t.Fatalf("runsc pause: %v: %s", err, out)
	}

	status, err := h.reopen(t).Status(t.Context(), source.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status of a source a cut fork left frozen = %+v, %v, want it thawed and running", status, err)
	}
	assertRunsUnmarked(t, h, source.ID)
	execIn(t, h, source.ID, "true")
}

// A fork over an id that is live would unmount the rootfs it runs on, so it is refused, and the source is never frozen (SHARD-457).
func TestForkRefusesAnIdThatIsLive(t *testing.T) {
	h := newHarness(t)
	source := h.start(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	sentry := h.sentryPID(t, source.ID)

	live := h.start(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	err := h.provider.Fork(t.Context(), source.ID, live)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Fork over a live id returned %v, want a refusal", err)
	}
	assertAlive(t, h, live.ID, true)
	assertMounted(t, h, live.ID, true)
	assertRunsUnmarked(t, h, source.ID)
	if got := h.sentryPID(t, source.ID); got != sentry {
		t.Errorf("the source's sentry is pid %d after the refusal, want the same %d", got, sentry)
	}

	ctx := context.Background()
	if err := h.provider.Stop(ctx, live.ID, stopGrace); err != nil {
		t.Fatalf("Stop after the refusal: %v", err)
	}
	if got := fmt.Sprint(h.provider.Remove(ctx, live.ID)); got != "<nil>" {
		t.Errorf("Remove after the refusal: %s", got)
	}
}

// assertRunsUnmarked proves runsc reports the source running and no fork mark is left on it.
func assertRunsUnmarked(t *testing.T, h *harness, id string) {
	t.Helper()

	out, err := h.runsc(t, "state", id).Output()
	if err != nil {
		t.Fatalf("runsc state %s: %v", id, err)
	}
	var state struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &state); err != nil {
		t.Fatalf("decode the state of %s: %v", id, err)
	}
	if state.Status != "running" {
		t.Errorf("runsc reports %s as %q, want running", id, state.Status)
	}
	if _, err := os.Stat(filepath.Join(stateDirOf(t, h, id), "fork-frozen")); err == nil {
		t.Errorf("sandbox %s still carries its fork mark", id)
	}
}
