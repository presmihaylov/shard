package sandbox_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

var killedAt = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// owed is a record an OOM kill stopped, whose start again falls due at the given time.
func owed(due time.Time, inARow int) models.Sandbox {
	sb := running()
	sb.State = models.StateStopped
	sb.PID = 0
	sb.StoppedReason = sandbox.OOMKilledReason
	sb.OOM = &models.OOM{Kills: inARow, InARow: inARow, KilledAt: killedAt, RestartAt: due}

	return sb
}

func TestOOMBackoffStartsAtOnceThenDoublesFromTenSecondsToFiveMinutes(t *testing.T) {
	want := []time.Duration{0, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, wait := range want {
		if got := sandbox.OOMBackoff(i + 1); got != wait {
			t.Errorf("the wait after kill %d in a row is %s, want %s", i+1, got, wait)
		}
	}
	if got := sandbox.OOMBackoff(1000); got != sandbox.OOMRestartBackoffCap {
		t.Errorf("the wait after a thousand kills in a row is %s, want the cap %s", got, sandbox.OOMRestartBackoffCap)
	}
}

func TestLivenessStartsAnOOMKilledSandboxAgainOnceDue(t *testing.T) {
	lab := newLivenessLab(t, owed(killedAt, 1), oomKilled())

	if err := lab.tickAt(t, lab.l.repo.sb, killedAt); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if !lab.l.provider.started || got.State != models.StateRunning || got.PID != 7 || got.StoppedReason != "" {
		t.Errorf("the record says %s with pid %d and the reason %q, want running with pid 7 and no reason: %v", got.State, got.PID, got.StoppedReason, lab.r.calls)
	}
	// ls and inspect keep the kill once the reason is gone, and the start is no longer owed.
	if got.OOM == nil || got.OOM.Kills != 1 || got.OOM.KilledAt != killedAt || got.OOM.RestartDue() {
		t.Errorf("the record holds the OOM %+v, want one kill at %s and no start owed", got.OOM, killedAt)
	}
	if len(lab.reports) != 1 || !strings.Contains(lab.reports[0], "started again") {
		t.Errorf("the pass reported %v, want one line on the start again", lab.reports)
	}
}

// The start again is the daemon's own, so it brings back what a daemon start does and no more (SHARD-790).
func TestAnOOMStartAgainRunsTheProcessesADaemonStartBringsBack(t *testing.T) {
	sb := withProcesses(owed(killedAt, 1), proc("web", models.RestartUnlessStopped), proc("job", models.RestartNo), proc("api", models.RestartOnFailure))
	lab := newLivenessLab(t, sb, oomKilled())

	if err := lab.tickAt(t, lab.l.repo.sb, killedAt); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if got := specNames(lab.l.provider.specs); !slices.Equal(got, []string{"web"}) {
		t.Errorf("the start again ran %v, want web alone", got)
	}
}

// The guest that comes back never knew the old run, so a process the start again leaves down must not hold its name.
func TestAnOOMKillEndsTheProcessesTheStartAgainLeavesDown(t *testing.T) {
	sb := withProcesses(running(), proc("web", models.RestartUnlessStopped), proc("job", models.RestartNo), proc("api", models.RestartOnFailure))
	lab := newLivenessLab(t, sb, oomKilled())

	for _, pass := range []string{"the kill", "the start again"} {
		if err := lab.tickAt(t, lab.l.repo.sb, killedAt); err != nil {
			t.Fatalf("Liveness, %s: %v", pass, err)
		}
	}

	for _, name := range []string{"job", "api"} {
		if _, err := lab.svc.Run(t.Context(), "sandbox1", sandbox.RunRequest{Name: name, Command: []string{name}}); err != nil {
			t.Errorf("a run of %s after the start again: %v", name, err)
		}
	}
}

func TestLivenessWaitsOutTheBackoffBeforeAStartAgain(t *testing.T) {
	due := killedAt.Add(20 * time.Second)
	lab := newLivenessLab(t, owed(due, 3), oomKilled())

	if err := lab.tickAt(t, lab.l.repo.sb, due.Add(-time.Second)); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if lab.l.provider.started || slices.Contains(lab.r.calls, "repo.Get") {
		t.Errorf("a tick before the backoff ran out touched the sandbox: %v", lab.r.calls)
	}

	if err := lab.tickAt(t, lab.l.repo.sb, due); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if !lab.l.provider.started || lab.l.repo.sb.State != models.StateRunning {
		t.Errorf("the tick at the due time left the record %s, want it started again", lab.l.repo.sb.State)
	}
}

// gVisor takes half a minute to die of its memory, so a run that short must still grow the backoff (SHARD-188).
func TestAnOOMSoonAfterAStartAgainGrowsTheBackoff(t *testing.T) {
	sb := running()
	sb.RunStartedAt = killedAt.Add(-30 * time.Second)
	sb.OOM = &models.OOM{Kills: 2, InARow: 2, KilledAt: killedAt.Add(-time.Minute)}
	lab := newLivenessLab(t, sb, oomKilled())

	if err := lab.tickAt(t, sb, killedAt); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	want := models.OOM{Kills: 3, InARow: 3, KilledAt: killedAt, RestartAt: killedAt.Add(20 * time.Second)}
	if got := lab.l.repo.sb.OOM; got == nil || *got != want {
		t.Errorf("the record holds the OOM %+v, want %+v", got, want)
	}
}

func TestAnOOMAfterAHealthyRunStartsTheBackoffOver(t *testing.T) {
	sb := running()
	sb.RunStartedAt = killedAt.Add(-sandbox.OOMHealthyRun)
	sb.OOM = &models.OOM{Kills: 9, InARow: 9, KilledAt: killedAt.Add(-time.Hour)}
	lab := newLivenessLab(t, sb, oomKilled())

	if err := lab.tickAt(t, sb, killedAt); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	want := models.OOM{Kills: 10, InARow: 1, KilledAt: killedAt, RestartAt: killedAt}
	if got := lab.l.repo.sb.OOM; got == nil || *got != want {
		t.Errorf("the record holds the OOM %+v, want %+v", got, want)
	}
}

func TestAStopCallsOffTheStartAgainAndTheReasonStays(t *testing.T) {
	lab := newLivenessLab(t, owed(killedAt.Add(time.Minute), 2), oomKilled())

	if _, err := lab.svc.Stop(t.Context(), "sandbox1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got := lab.l.repo.sb
	if got.State != models.StateStopped || got.OOM.RestartDue() || got.StoppedReason != sandbox.OOMKilledReason {
		t.Errorf("after the stop the record says %s with the OOM %+v and the reason %q, want stopped with %q and no start owed", got.State, got.OOM, got.StoppedReason, sandbox.OOMKilledReason)
	}

	if err := lab.tickAt(t, got, killedAt.Add(time.Hour)); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if lab.l.provider.started {
		t.Errorf("a sandbox the operator stopped was started again: %v", lab.r.calls)
	}
}

// The list may be a tick old, so a stop that called the start off since is read from the store first.
func TestLivenessNeverStartsAgainWhatAStopCalledOffSinceTheList(t *testing.T) {
	listed := owed(killedAt, 1)
	stored := listed
	stored.OOM = &models.OOM{Kills: 1, InARow: 1, KilledAt: killedAt}
	lab := newLivenessLab(t, stored, oomKilled())

	if err := lab.tickAt(t, listed, killedAt); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if lab.l.provider.started {
		t.Errorf("a start the store no longer owes ran off the old list: %v", lab.r.calls)
	}
}

func TestLivenessNeverStartsAgainASandboxRemovedSinceTheList(t *testing.T) {
	lab := newLivenessLab(t, owed(killedAt, 1), oomKilled())
	lab.l.repo.missing = true

	if err := lab.tickAt(t, owed(killedAt, 1), killedAt); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if lab.l.provider.started {
		t.Errorf("a removed sandbox was started again: %v", lab.r.calls)
	}
}

func TestAStartBeforeTheBackoffRanOutTakesThePlaceOfTheStartAgain(t *testing.T) {
	lab := newLivenessLab(t, owed(killedAt.Add(time.Minute), 2), oomKilled())

	if _, err := lab.svc.Start(t.Context(), "sandbox1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := lab.l.repo.sb; got.State != models.StateRunning || got.OOM.RestartDue() {
		t.Errorf("after the start the record says %s with the OOM %+v, want running and no start owed", got.State, got.OOM)
	}
}

func TestAFailedStartAgainPutsTheNextOneABackoffOn(t *testing.T) {
	lab := newLivenessLab(t, owed(killedAt, 1), oomKilled())
	lab.l.provider.startErr = errors.New("runsc: no space left on device")

	err := lab.tickAt(t, lab.l.repo.sb, killedAt)
	if err == nil || !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("Liveness answered %v, want the start's own error", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StateStopped || got.OOM == nil || got.OOM.InARow != 2 || got.OOM.RestartAt != killedAt.Add(sandbox.OOMRestartBackoff) {
		t.Errorf("the record says %s with the OOM %+v, want stopped with the next start %s on", got.State, got.OOM, sandbox.OOMRestartBackoff)
	}
	if len(lab.reports) != 1 || !strings.Contains(lab.reports[0], "tries again in 10s") {
		t.Errorf("the pass reported %v, want one line on the next try", lab.reports)
	}
}

// ls hides a stopped sandbox unless --all, but one the daemon is about to start again is not done.
func TestListShowsAStoppedSandboxTheDaemonStartsAgain(t *testing.T) {
	gone := stopped()
	gone.ID = "sandbox2"
	r := &recorder{}
	_, l := newService(t, r, owed(killedAt, 1))
	l.repo.left = []models.Sandbox{owed(killedAt, 1), gone}

	got, err := sandbox.List(l.repo, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "sandbox1" {
		t.Errorf("ls lists %v, want only the sandbox owed a start", got)
	}
}
