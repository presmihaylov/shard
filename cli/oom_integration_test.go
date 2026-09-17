//go:build integration

package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// oomBomb doubles strings in anonymous memory in 32 tasks, because memory.high throttles each one to ~128 KiB/s past the bound.
const oomBomb = `i=0; while [ $i -lt 32 ]; do awk 'BEGIN { s = "x"; while (1) s = s s }' & i=$((i+1)); done; wait`

// oomBudget covers several kills of ~30 s each with their backoff, at one tick every 5 s, three sandboxes at once.
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
	if !strings.Contains(daemonUnderTest.logged(), "sandbox "+id+" "+sandbox.OOMKilledReason+": started again, 1") {
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

// A slow OOM loop runs well past the reset window each time, so a finite limit clears before it is spent.
func TestTheDaemonKeepsALimitedOOMLoopAliveAcrossHealthyRuns(t *testing.T) {
	app, out := newCreateApp(t)
	t.Parallel()

	id := createBoundMax(t, app, out, 2, oomBomb)

	// A limit of 2 with no reset would give up on the third kill, so a third start again proves the reset.
	marker := "sandbox " + id + " " + sandbox.OOMKilledReason + ": started again, 1 of 2"
	awaitLog(t, func() bool { return strings.Count(daemonUnderTest.logged(), marker) >= 3 })

	sb := awaitRecord(t, app, id, func(sb models.Sandbox) bool { return sb.State == models.StateRunning })
	if strings.Contains(sb.StoppedReason, "are spent") {
		t.Errorf("the record gave up with %q, want the reset to keep the limit unspent", sb.StoppedReason)
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

// createBoundMax makes a bounded sandbox whose OOM restart is capped at max starts in a row.
func createBoundMax(t *testing.T, app App, out *bytes.Buffer, max int, script string) string {
	t.Helper()

	args := []string{"create", "--memory", "64", fmt.Sprintf("--restart-on-oom=%d", max), testImage, "--", "/bin/sh", "-c", script}
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("create: %v", err)
	}

	id := strings.TrimSpace(out.String())
	out.Reset()
	t.Cleanup(func() { cleanUp(t, app, id) })

	return id
}

// awaitLog polls the daemon log until the condition holds, within the same budget as awaitRecord.
func awaitLog(t *testing.T, ready func() bool) {
	t.Helper()

	deadline := time.Now().Add(oomBudget)
	for {
		if ready() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon log never read as wanted in %s; last log:\n%s", oomBudget, daemonUnderTest.logged())
		}

		time.Sleep(time.Second)
	}
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
