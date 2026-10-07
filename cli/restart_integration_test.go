//go:build integration

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// restartBudget covers six starts again whose backoff doubles from 1 s to 32 s, the 1 s tick and a slow box.
const restartBudget = 150 * time.Second

func TestTheSupervisorStartsAProcessAgainUntilThePolicyGivesUp(t *testing.T) {
	app, out := newCreateApp(t)
	t.Parallel()

	id := runDetachedWith(t, app, out, nil, []string{"--restart", "on-failure", "--restart-retries", "2", "--restart-backoff", "1s"}, "/bin/sh", "-c", "exit 1")
	t.Cleanup(func() { cleanUp(t, app, id) })

	sb, p := awaitRestarts(t, app, id, func(s models.ProcessStatus) bool { return s.State == models.ProcessGaveUp })
	if sb.State != models.StateRunning || p.Status.Restarts != 2 || p.Status.Exit == nil || p.Status.Exit.Code != 1 {
		t.Errorf("the record reads %s with %+v, want running with the process given up after 2 starts again", sb.State, p.Status)
	}

	if err := app.Run(t.Context(), []string{"ps", id}); err != nil {
		t.Fatalf("ps: %v", err)
	}
	if got := strings.Join(strings.Fields(out.String()), " "); !strings.Contains(got, processName+" gave-up 2/2 1 on-failure") {
		t.Errorf("ps printed %q, want the process given up after 2 of 2", out.String())
	}
	out.Reset()

	for _, line := range []string{
		"sandbox " + id + ": process " + processName + " was started again, 2 of 2",
		"sandbox " + id + ": process " + processName + " exited again and the 2 restarts its policy allows are spent",
	} {
		if !strings.Contains(daemonUnderTest.logged(), line) {
			t.Errorf("the daemon logged no %q", line)
		}
	}

	if err := app.Run(t.Context(), []string{"stop", id}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	sb = record(t, app, id)
	if p := processOf(t, sb); sb.State != models.StateStopped || p.Status.State != models.ProcessGaveUp || p.Status.Exit == nil || p.Status.Exit.Code != 1 || p.Status.Restarts != 2 {
		t.Errorf("the stopped record reads %s with %+v, want the give-up, the last exit and the count kept", sb.State, p.Status)
	}
}

// always never gives up, so a clean exit starts the process again past the old default of five.
func TestAlwaysStartsAProcessAgainWithoutEnd(t *testing.T) {
	app, out := newCreateApp(t)
	t.Parallel()

	id := runDetachedWith(t, app, out, nil, []string{"--restart", "always", "--restart-backoff", "1s"}, "/bin/sh", "-c", "exit 0")
	t.Cleanup(func() { cleanUp(t, app, id) })

	// The old default gave up at five, so a sixth start again with no give-up proves always never does.
	sb, p := awaitRestarts(t, app, id, func(s models.ProcessStatus) bool { return s.Restarts >= 6 })
	if sb.State != models.StateRunning || p.Status.State.Ended() {
		t.Errorf("the record reads %s with %+v, want running with no end under always", sb.State, p.Status)
	}

	if err := app.Run(t.Context(), []string{"ps", id}); err != nil {
		t.Fatalf("ps: %v", err)
	}
	// always shows the count with no limit beside it and never a give-up.
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if row := strings.Fields(lines[len(lines)-1]); len(row) < 5 || row[1] == string(models.ProcessGaveUp) || strings.Contains(row[2], "/") || row[4] != "always" {
		t.Errorf("ps printed %q, want the always count with no limit and no give-up", out.String())
	}
}

// awaitRestarts polls the record the daemon's tick writes until the process reads as wanted, and names what it last saw if never.
func awaitRestarts(t *testing.T, app App, id string, want func(models.ProcessStatus) bool) (models.Sandbox, models.Process) {
	t.Helper()

	deadline := time.Now().Add(restartBudget)
	for {
		sb := record(t, app, id)
		p := processOf(t, sb)
		if want(p.Status) {
			return sb, p
		}
		if time.Now().After(deadline) {
			t.Fatalf("the record of %s never read as wanted in %s, last %s with %+v", id, restartBudget, sb.State, p.Status)
		}

		time.Sleep(time.Second)
	}
}
