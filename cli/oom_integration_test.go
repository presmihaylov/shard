//go:build integration

package cli

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// oomBomb doubles strings in anonymous memory in 32 tasks, because memory.high throttles each one to ~128 KiB/s past the bound.
const oomBomb = `i=0; while [ $i -lt 32 ]; do awk 'BEGIN { s = "x"; while (1) s = s s }' & i=$((i+1)); done; wait`

// oomBudget covers one kill of ~30 s, at one tick every 5 s, with room for a slow host.
const oomBudget = 3 * time.Minute

// The host ends the sandbox for its memory, and only a start brings it back, over the files it kept (SHARD-461).
func TestTheDaemonStopsAnOOMKilledSandboxAndAStartBringsItBack(t *testing.T) {
	app, out := newCreateApp(t)

	// The guest overruns its bound on the first run only, so the run a start brings back can be used.
	script := "if [ ! -e /ran ]; then touch /ran; " + oomBomb + "; fi; while true; do sleep 1; done"
	id := runDetachedWith(t, app, out, "--memory", oomBound(), testImage, "--", "/bin/sh", "-c", script)
	t.Cleanup(func() { cleanUp(t, app, id) })

	sb := awaitRecord(t, app, id, func(sb models.Sandbox) bool { return sb.State == models.StateStopped })
	if sb.StoppedReason != sandbox.OOMKilledReason || sb.PID != 0 {
		t.Errorf("the record says %q with pid %d, want the kill named and no pid", sb.StoppedReason, sb.PID)
	}
	if !strings.Contains(daemonUnderTest.logged(), "sandbox "+id+": "+sandbox.OOMKilledReason+", the record now says stopped") {
		t.Error("the daemon logged no line for the stop")
	}

	// Two ticks pass, and nothing starts it again.
	time.Sleep(11 * time.Second)
	if got := record(t, app, id); got.State != models.StateStopped {
		t.Fatalf("the record says %s two ticks after the kill, want it still stopped", got.State)
	}

	if err := app.Run(t.Context(), []string{"start", id}); err != nil {
		t.Fatalf("start after the kill: %v", err)
	}
	if got, err := runExec(t, app, "exec", id, "--", "/bin/ls", "/ran"); err != nil || !strings.Contains(got, "/ran") {
		t.Errorf("the run a start brought back lost the file its first run wrote: %q, %v", got, err)
	}
}

// oomBound is the smallest --memory the suite's provider takes, so the bomb meets the bound soonest.
func oomBound() string {
	if bound := itestResources().MemoryMiB; bound != nil {
		return strconv.FormatInt(*bound, 10) + "MiB"
	}

	return "64MiB"
}

// awaitRecord polls the daemon until the record reads as wanted, and names the record it last saw if never.
func awaitRecord(t *testing.T, app App, id string, ready func(models.Sandbox) bool) models.Sandbox {
	t.Helper()

	deadline := time.Now().Add(oomBudget)
	for {
		sb := record(t, app, id)
		if ready(sb) {
			return sb
		}
		if time.Now().After(deadline) {
			t.Fatalf("the record of %s never read as wanted in %s, last %s (%q)", id, oomBudget, sb.State, sb.StoppedReason)
		}

		time.Sleep(time.Second)
	}
}
