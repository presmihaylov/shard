//go:build integration

package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// oomBomb doubles strings in anonymous memory in 32 tasks, because memory.high throttles each one to ~128 KiB/s past the bound.
const oomBomb = `i=0; while [ $i -lt 32 ]; do awk 'BEGIN { s = "x"; while (1) s = s s }' & i=$((i+1)); done; wait`

// oomBudget covers six kills of ~30 s each with their backoff, at one tick every 5 s, three sandboxes at once.
const oomBudget = 6 * time.Minute

// The three tests share the daemon and run side by side, because each one waits on the 5 s tick.
func TestTheDaemonBringsBackAnOOMKilledSandboxThatAskedForIt(t *testing.T) {
	app, out := newCreateApp(t)
	t.Parallel()

	// The guest overruns its bound on the first run only, so the sandbox it comes back as can be used.
	script := "if [ ! -e /ran ]; then touch /ran; " + oomBomb + "; fi; while true; do sleep 1; done"
	id := createBound(t, app, out, true, script)

	sb := awaitRecord(t, app, id, func(sb models.Sandbox) bool { return sb.OOMRestarts == 1 && sb.State == models.StateRunning })
	if sb.StoppedReason != "" {
		t.Errorf("the record keeps the reason %q after the start again", sb.StoppedReason)
	}
	if got, err := runExec(t, app, "exec", id, "--", "/bin/echo", "alive"); err != nil || !strings.Contains(got, "alive") {
		t.Errorf("exec in the sandbox that came back = %q, %v", got, err)
	}
	if !strings.Contains(daemonUnderTest.logged(), "sandbox "+id+" "+sandbox.OOMKilledReason+": started again, 1 of 5") {
		t.Error("the daemon logged no line for the start again")
	}
}

func TestTheDaemonLeavesAnOOMKilledSandboxThatDidNotAsk(t *testing.T) {
	app, out := newCreateApp(t)
	t.Parallel()

	id := createBound(t, app, out, false, oomBomb)

	sb := awaitRecord(t, app, id, func(sb models.Sandbox) bool { return sb.State == models.StateStopped })
	if sb.StoppedReason != sandbox.OOMKilledReason || sb.OOMRestarts != 0 {
		t.Errorf("the record says %q with %d starts again, want the kill named and none", sb.StoppedReason, sb.OOMRestarts)
	}
}

func TestTheDaemonGivesUpOnASandboxThatOverrunsEveryTime(t *testing.T) {
	app, out := newCreateApp(t)
	t.Parallel()

	id := createBound(t, app, out, true, oomBomb)

	sb := awaitRecord(t, app, id, func(sb models.Sandbox) bool {
		return sb.State == models.StateStopped && sb.OOMRestarts == sandbox.OOMRestartCap
	})
	if !strings.Contains(sb.StoppedReason, "the 5 starts again the cap allows are spent") {
		t.Errorf("the record says %q, want the cap named", sb.StoppedReason)
	}
}

// createBound makes a sandbox with the smallest bound the daemon takes, and the restart policy when asked.
func createBound(t *testing.T, app App, out *bytes.Buffer, restart bool, script string) string {
	t.Helper()

	args := []string{"create", "--memory", "64"}
	if restart {
		args = append(args, "--restart-on-oom")
	}
	args = append(args, testImage, "--", "/bin/sh", "-c", script)
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("create: %v", err)
	}

	id := strings.TrimSpace(out.String())
	out.Reset()
	t.Cleanup(func() { cleanUp(t, app, id) })

	return id
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
			t.Fatalf("the record of %s never read as wanted in %s, last %s (%q) with %d starts again", id, oomBudget, sb.State, sb.StoppedReason, sb.OOMRestarts)
		}

		time.Sleep(time.Second)
	}
}
