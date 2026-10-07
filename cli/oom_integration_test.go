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

// oomBudget covers two kills of ~30 s, at one tick every 5 s, with room for a slow host.
const oomBudget = 3 * time.Minute

// The host ends the sandbox for its memory, and the daemon starts it again on its own, over the files it kept (SHARD-786).
func TestTheDaemonStartsAnOOMKilledSandboxAgain(t *testing.T) {
	app, out := newCreateApp(t)

	// The process never starts again, so the run the daemon brings back holds no bomb and can be used.
	id := runDetachedWith(t, app, out, []string{"--memory", oomBound()}, []string{"--restart", "no"}, "/bin/sh", "-c", "touch /ran; "+oomBomb)
	t.Cleanup(func() { cleanUp(t, app, id) })

	sb := awaitRecord(t, app, id, func(sb models.Sandbox) bool { return sb.State == models.StateRunning && sb.OOM != nil })
	if sb.OOM.Kills != 1 || sb.OOM.RestartDue() || sb.StoppedReason != "" {
		t.Errorf("the record holds the OOM %+v and the reason %q, want one kill, no start owed and no reason", sb.OOM, sb.StoppedReason)
	}
	logged := daemonUnderTest.logged()
	for _, line := range []string{"sandbox " + id + ": " + sandbox.OOMKilledReason + ", the record now says stopped and the daemon starts it again now (kill 1, 1 in a row)", "sandbox " + id + ": started again after it ran out of memory (kill 1, 1 in a row)"} {
		if !strings.Contains(logged, line) {
			t.Errorf("the daemon logged no line %q", line)
		}
	}

	if got, err := runExec(t, app, "exec", id, "--", "/bin/ls", "/ran"); err != nil || !strings.Contains(got, "/ran") {
		t.Errorf("the run the daemon brought back lost the file its first run wrote: %q, %v", got, err)
	}
}

// A sandbox that runs out of memory at boot waits a growing backoff, and a stop calls the start again off.
func TestAStopCallsOffTheStartAgainOfASandboxThatKeepsRunningOutOfMemory(t *testing.T) {
	app, out := newCreateApp(t)

	// unless-stopped, the default, runs the bomb again on each start again.
	id := runDetachedWith(t, app, out, []string{"--memory", oomBound()}, nil, "/bin/sh", "-c", oomBomb)
	t.Cleanup(func() { cleanUp(t, app, id) })

	sb := awaitRecord(t, app, id, func(sb models.Sandbox) bool { return sb.OOM != nil && sb.OOM.Kills == 2 })
	if sb.OOM.InARow != 2 || sb.OOM.RestartAt.Sub(sb.OOM.KilledAt) != sandbox.OOMRestartBackoff {
		t.Errorf("the record holds the OOM %+v, want the second kill in a row to wait %s", sb.OOM, sandbox.OOMRestartBackoff)
	}

	if err := app.Run(t.Context(), []string{"stop", id}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Past the backoff and two ticks, nothing starts it again.
	time.Sleep(sandbox.OOMRestartBackoff + 11*time.Second)
	if got := record(t, app, id); got.State != models.StateStopped || got.OOM.RestartDue() || got.OOM.Kills != 2 {
		t.Errorf("after the stop the record says %s with the OOM %+v, want stopped with two kills and no start owed", got.State, got.OOM)
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
