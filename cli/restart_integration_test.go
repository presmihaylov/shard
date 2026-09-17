//go:build integration

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// restartBudget covers two starts again with a 1 s then 2 s backoff, the 1 s tick and a slow box.
const restartBudget = 90 * time.Second

func TestTheSupervisorStartsTheEntrypointAgainUntilThePolicyGivesUp(t *testing.T) {
	app, out := newCreateApp(t)
	t.Parallel()

	args := []string{"create", "--restart", "on-failure", "--restart-retries", "2", "--restart-backoff", "1s", testImage, "--", "/bin/sh", "-c", "exit 1"}
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("create: %v", err)
	}
	id := strings.TrimSpace(out.String())
	out.Reset()
	t.Cleanup(func() { cleanUp(t, app, id) })

	sb := awaitRestarts(t, app, id, func(c models.RestartCount) bool { return c.GaveUp })
	if sb.State != models.StateRunning || sb.Restart.Count != 2 || sb.Restart.LastAt.IsZero() {
		t.Errorf("the record reads %s with %+v, want running with the 2 starts again named", sb.State, sb.Restart)
	}
	for _, line := range []string{
		"sandbox " + id + ": the entrypoint was started again, 2 of 2",
		"sandbox " + id + ": the entrypoint exited again and the 2 starts again the policy allows are spent",
	} {
		if !strings.Contains(daemonUnderTest.logged(), line) {
			t.Errorf("the daemon logged no %q", line)
		}
	}

	if err := app.Run(t.Context(), []string{"ls"}); err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(out.String(), "on-failure 2/2 gave up") {
		t.Errorf("ls printed %q, want the restart column", out.String())
	}

	if err := app.Run(t.Context(), []string{"stop", id}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	sb = record(t, app, id)
	if sb.State != models.StateStopped || sb.ExitStatus == nil || sb.ExitStatus.Code != 1 || sb.Restart.Count != 2 || !sb.Restart.GaveUp {
		t.Errorf("the stopped record reads %s with exit %+v and %+v, want the last exit and the count kept", sb.State, sb.ExitStatus, sb.Restart)
	}
}

// awaitRestarts polls the daemon until the count reads as wanted, and names what it last saw if never.
func awaitRestarts(t *testing.T, app App, id string, want func(models.RestartCount) bool) models.Sandbox {
	t.Helper()

	deadline := time.Now().Add(restartBudget)
	for {
		sb := record(t, app, id)
		if sb.Restart != nil && want(sb.Restart.RestartCount) {
			return sb
		}
		if time.Now().After(deadline) {
			t.Fatalf("the record of %s never counted as wanted in %s, last %s with %+v", id, restartBudget, sb.State, sb.Restart)
		}

		time.Sleep(time.Second)
	}
}
