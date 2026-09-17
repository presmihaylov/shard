package sandbox_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// oomKilled is what the substrate says once the host ended the sandbox for its memory: held, not alive.
func oomKilled() models.Status {
	return models.Status{Exists: true, State: models.StateStopped, OOMKilled: true}
}

type livenessLab struct {
	svc     *sandbox.Service
	l       layers
	r       *recorder
	reports []string
}

func newLivenessLab(t *testing.T, sb models.Sandbox, status models.Status) *livenessLab {
	t.Helper()

	lab := &livenessLab{r: &recorder{}}
	lab.svc, lab.l = newService(t, lab.r, sb)
	lab.l.provider.status = status

	return lab
}

// tick runs one pass over the record as the daemon lists it, which may be older than what the store holds.
func (l *livenessLab) tick(t *testing.T, listed models.Sandbox, now time.Time) error {
	t.Helper()

	return l.svc.Liveness(t.Context(), []models.Sandbox{listed}, now, func(line string) { l.reports = append(l.reports, line) })
}

func optedIn() models.Sandbox {
	sb := running()
	sb.Resources = models.Resources{MemoryMiB: 64}
	sb.RestartOnOOM = true

	return sb
}

func TestLivenessRecordsAnEntrypointExitAndLeavesTheSandboxRunning(t *testing.T) {
	lab := newLivenessLab(t, running(), alive(42))
	lab.l.provider.entrypointExit = &models.ExitStatus{Code: 7}

	if err := lab.tick(t, running(), time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StateRunning || got.ExitStatus == nil || *got.ExitStatus != (models.ExitStatus{Code: 7}) {
		t.Errorf("the record says %s with exit %+v, want running with {code:7}", got.State, got.ExitStatus)
	}
	if len(lab.reports) != 1 || !strings.Contains(lab.reports[0], "entrypoint exited") {
		t.Errorf("the pass reported %v, want one line on the exit", lab.reports)
	}
}

func TestLivenessLeavesARunningEntrypointAlone(t *testing.T) {
	lab := newLivenessLab(t, running(), alive(42))

	if err := lab.tick(t, running(), time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	if got := lab.l.repo.sb; got.State != models.StateRunning || got.ExitStatus != nil || len(lab.reports) != 0 {
		t.Errorf("a running entrypoint was touched: %+v, reports %v", got, lab.reports)
	}
}

func TestLivenessNeverRewritesARecordedExit(t *testing.T) {
	sb := running()
	sb.ExitStatus = &models.ExitStatus{Code: 7}
	lab := newLivenessLab(t, sb, alive(42))
	lab.l.provider.entrypointExit = &models.ExitStatus{Code: 7}

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	if len(lab.reports) != 0 {
		t.Errorf("the pass reported %v over an exit it already knew", lab.reports)
	}
}

func TestLivenessStopsASandboxWhoseProcessDied(t *testing.T) {
	lab := newLivenessLab(t, running(), gone())

	if err := lab.tick(t, running(), time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StateStopped || got.PID != 0 || got.StoppedReason != sandbox.DiedReason {
		t.Errorf("the record says %s with pid %d and the reason %q, want stopped with %q", got.State, got.PID, got.StoppedReason, sandbox.DiedReason)
	}
	if lab.l.provider.started {
		t.Error("a sandbox that only died was started again")
	}
	if len(lab.reports) != 1 || !strings.Contains(lab.reports[0], sandbox.DiedReason) {
		t.Errorf("the pass reported %v, want one line on the death", lab.reports)
	}
}

func TestLivenessStartsASandboxThatAskedForItAfterOOM(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	lab := newLivenessLab(t, optedIn(), oomKilled())

	if err := lab.tick(t, optedIn(), now); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StateRunning || got.PID != 7 || got.StoppedReason != "" {
		t.Errorf("the record says %s with pid %d and the reason %q, want running with the new pid and no reason", got.State, got.PID, got.StoppedReason)
	}
	if got.OOMRestarts != 1 || !got.OOMRestartedAt.Equal(now) {
		t.Errorf("the record counts %d starts again at %v, want 1 at %v", got.OOMRestarts, got.OOMRestartedAt, now)
	}
	// The namespace goes up again before the sandbox does, as a start by hand does it.
	want := []string{"net.Allocate", "provider.Start"}
	if calls := keep(lab.r.calls, want...); !slices.Equal(calls, want) {
		t.Errorf("the sandbox was driven as %v, want %v", calls, want)
	}
	if len(lab.reports) != 1 || !strings.Contains(lab.reports[0], "started again, 1") {
		t.Errorf("the pass reported %v, want one line counting the start", lab.reports)
	}
}

func TestLivenessStopsTheRecordOfASandboxThatDidNotAskAfterOOM(t *testing.T) {
	sb := running()
	sb.Resources = models.Resources{MemoryMiB: 64}
	lab := newLivenessLab(t, sb, oomKilled())

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StateStopped || got.PID != 0 || got.StoppedReason != sandbox.OOMKilledReason {
		t.Errorf("the record says %s with pid %d and the reason %q, want stopped with %q", got.State, got.PID, got.StoppedReason, sandbox.OOMKilledReason)
	}
	if lab.l.provider.started || got.OOMRestarts != 0 {
		t.Error("a sandbox that never asked was started again")
	}
	if len(lab.reports) != 1 || !strings.Contains(lab.reports[0], "the record now says stopped") {
		t.Errorf("the pass reported %v, want one line on the stop", lab.reports)
	}
}

func TestLivenessGivesUpAtTheOOMLimit(t *testing.T) {
	sb := optedIn()
	sb.MaxOOMRestarts = 5
	sb.OOMRestarts = 5
	lab := newLivenessLab(t, sb, oomKilled())

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StateStopped || !strings.Contains(got.StoppedReason, "the 5 starts again the limit allows are spent") {
		t.Errorf("the record says %s with the reason %q, want stopped with the limit named", got.State, got.StoppedReason)
	}
	if lab.l.provider.started || got.OOMRestarts != 5 {
		t.Error("a sandbox at the limit was started again")
	}
}

// A healthy run of the reset window clears the count, so a slow OOM loop never spends a finite limit.
func TestLivenessResetsTheOOMCountAfterAHealthyRun(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	sb := optedIn()
	sb.MaxOOMRestarts = 3
	sb.OOMRestarts = 3
	sb.StartedAt = now.Add(-sandbox.OOMHealthyRun)
	sb.OOMRestartedAt = now.Add(-time.Hour)
	lab := newLivenessLab(t, sb, oomKilled())

	if err := lab.tick(t, sb, now); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StateRunning || got.OOMRestarts != 1 {
		t.Errorf("the record says %s with %d starts again, want running with the count reset then 1", got.State, got.OOMRestarts)
	}
	if len(lab.reports) != 1 || !strings.Contains(lab.reports[0], "started again, 1 of 3") {
		t.Errorf("the pass reported %v, want the count reset to 1 of 3", lab.reports)
	}
}

func TestLivenessWaitsOutTheOOMBackoff(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	sb := optedIn()
	sb.OOMRestarts = 2
	sb.OOMRestartedAt = now.Add(-time.Second)
	lab := newLivenessLab(t, sb, oomKilled())

	// Two starts again put the wait at 2 s, and only one has passed.
	if err := lab.tick(t, sb, now); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if got := lab.l.repo.sb; got.State != models.StateRunning || got.OOMRestarts != 2 || lab.l.provider.started {
		t.Errorf("the record says %s with %d starts again inside the wait, want it untouched", got.State, got.OOMRestarts)
	}
	if len(lab.reports) != 0 {
		t.Errorf("the pass reported %v inside the wait", lab.reports)
	}

	if err := lab.tick(t, sb, now.Add(time.Second)); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if got := lab.l.repo.sb; got.State != models.StateRunning || got.OOMRestarts != 3 || !lab.l.provider.started {
		t.Errorf("the record says %s with %d starts again once the wait passed, want running with 3", got.State, got.OOMRestarts)
	}
}

// The list may be a tick old, so a stop that landed since is read from the store before anything is asked.
func TestLivenessNeverTouchesASandboxTheRecordSaysStopped(t *testing.T) {
	sb := optedIn()
	sb.State = models.StateStopped
	sb.PID = 0
	lab := newLivenessLab(t, sb, oomKilled())

	if err := lab.tick(t, optedIn(), time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	if lab.l.provider.started || slices.Contains(lab.r.calls, "provider.Status") {
		t.Errorf("a stopped sandbox reached the substrate: %v", lab.r.calls)
	}
}

func TestLivenessCountsAnOOMStartThatFailed(t *testing.T) {
	lab := newLivenessLab(t, optedIn(), oomKilled())
	lab.r.fail = []string{"provider.Start"}

	err := lab.tick(t, optedIn(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "start sandbox sandbox1 again") {
		t.Fatalf("Liveness = %v, want the start's failure", err)
	}

	// The next tick sees the same kill and must not spend the cap on a substrate that refuses.
	if got := lab.l.repo.sb; got.State != models.StateStopped || got.OOMRestarts != 1 {
		t.Errorf("the record says %s with %d starts again, want stopped with the failed one counted", got.State, got.OOMRestarts)
	}
}
