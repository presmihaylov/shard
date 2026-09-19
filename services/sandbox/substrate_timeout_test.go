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

// A short budget so a wedged Status resolves fast, and a settle short enough to keep the test quick.
func fastBudget(c *sandbox.Config)  { c.ProbeBudget = 50 * time.Millisecond }
func shortSettle(c *sandbox.Config) { c.StopSettle = 100 * time.Millisecond }
func fastStart(c *sandbox.Config)   { c.StartBudget = 50 * time.Millisecond }

// bounded fails a test whose verb ran past the point a wedged substrate should have been cut off.
func bounded(t *testing.T, start time.Time, what string) {
	t.Helper()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("%s took %s, want it bounded well under a second", what, elapsed)
	}
}

// The liveness tick must never hold the lock across a wedged Status: it logs one line and leaves the record.
func TestLivenessLeavesTheRecordWhenTheSubstrateDoesNotAnswer(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running(), fastBudget)
	l.provider.statusGate = make(chan struct{})

	var reports []string
	start := time.Now()
	err := svc.Liveness(t.Context(), []models.Sandbox{running()}, time.Now(), func(line string) { reports = append(reports, line) })
	if err != nil {
		t.Fatalf("Liveness returned %v, want nil so the daemon task lives", err)
	}
	bounded(t, start, "the tick")

	if len(reports) != 1 || !strings.Contains(reports[0], "did not answer within") {
		t.Errorf("the tick reported %v, want one line on the wedged substrate", reports)
	}
	if got := l.repo.sb; got.State != models.StateRunning || got.PID != 42 {
		t.Errorf("the record is now %s pid %d, want it left running with pid 42", got.State, got.PID)
	}
}

// rm without force cannot read the sandbox, so it fails fast with the code the API answers 504 with.
func TestRemoveFailsFastWhenTheSubstrateDoesNotAnswer(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running(), fastBudget)
	l.provider.statusGate = make(chan struct{})

	start := time.Now()
	err := svc.Remove(t.Context(), "sandbox1", false, sandbox.DefaultStopGrace)
	bounded(t, start, "rm")

	var timeout *sandbox.SubstrateTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("Remove returned %v, want a SubstrateTimeoutError", err)
	}
	if timeout.Op != "rm" {
		t.Errorf("the error names op %q, want rm", timeout.Op)
	}
	if l.provider.stopped || l.repo.deleted {
		t.Error("rm without force killed or removed a sandbox it could not read")
	}
}

// A stopped record whose Status wedges must fall through to the stop that kills it, not fail on the check.
func TestStopFallsThroughToTheKillWhenAStoppedRecordStillLies(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, stopped(), fastBudget, shortSettle)
	l.provider.statusGate = make(chan struct{})
	l.provider.stopUnwedges = true

	start := time.Now()
	if _, err := svc.Stop(t.Context(), "sandbox1", sandbox.DefaultStopGrace); err != nil {
		t.Fatalf("Stop returned %v, want the kill path to complete", err)
	}
	bounded(t, start, "stop")

	if !l.provider.stopped {
		t.Error("the wedged stopped record never reached the kill path")
	}
}

// rm --force on a wedge is the one reclaim route: the raw kill frees the substrate, then the stop and the teardown run (SHARD-207b).
func TestRemoveForceReclaimsAWedgedSandboxThroughTheRawKill(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running(), fastBudget)
	l.provider.statusGate = make(chan struct{})

	start := time.Now()
	if err := svc.Remove(t.Context(), "sandbox1", true, sandbox.DefaultStopGrace); err != nil {
		t.Fatalf("Remove --force returned %v, want the kill to carry it through", err)
	}
	bounded(t, start, "rm --force")

	want := []string{"provider.Reclaim", "provider.Stop", "provider.Remove"}
	if got := keep(r.calls, want...); !slices.Equal(got, want) {
		t.Errorf("rm --force ran %v, want the kill, then the stop, then the teardown: %v", got, want)
	}
	if !l.repo.deleted {
		t.Error("the record of the reclaimed sandbox is still there")
	}
}

// plainProvider hides the raw kill, the way a substrate that offers none looks to the orchestrator.
type plainProvider struct{ models.Provider }

// A substrate without a raw kill leaves rm --force where it was: fast, typed, and reclaiming nothing.
func TestRemoveForceFailsFastWhenTheSubstrateOffersNoKill(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running(), fastBudget, func(c *sandbox.Config) { c.Provider = plainProvider{c.Provider} })
	l.provider.statusGate = make(chan struct{})

	start := time.Now()
	err := svc.Remove(t.Context(), "sandbox1", true, sandbox.DefaultStopGrace)
	bounded(t, start, "rm --force")

	var timeout *sandbox.SubstrateTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("Remove --force returned %v, want a SubstrateTimeoutError", err)
	}
	if timeout.Op != "rm" {
		t.Errorf("the error names op %q, want rm", timeout.Op)
	}
	if l.provider.stopped || l.provider.removed || l.repo.deleted {
		t.Errorf("rm --force acted on a wedged sandbox it had no kill for: stopped=%v removed=%v deleted=%v", l.provider.stopped, l.provider.removed, l.repo.deleted)
	}
}

// A kill that did not land keeps the wedge typed, so the API still answers 504, and says why the kill failed.
func TestRemoveForceReportsAKillThatDidNotLand(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running(), fastBudget)
	l.provider.statusGate = make(chan struct{})
	l.provider.reclaimErr = errors.New("sandbox sandbox1 still holds processes [4242] after SIGKILL")

	err := svc.Remove(t.Context(), "sandbox1", true, sandbox.DefaultStopGrace)

	var timeout *sandbox.SubstrateTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("Remove --force returned %v, want the SubstrateTimeoutError kept under the kill failure", err)
	}
	if !strings.Contains(err.Error(), "still holds processes [4242]") {
		t.Errorf("the error is %q, want it to say why the kill failed", err)
	}
	if l.provider.stopped || l.provider.removed || l.repo.deleted {
		t.Errorf("rm --force went on after a kill that failed: stopped=%v removed=%v deleted=%v", l.provider.stopped, l.provider.removed, l.repo.deleted)
	}
}

// A restart that meets a wedged runtime must not pin the serial liveness task: it fails fast and typed for
// that sandbox and leaves its record stopped, while every other sandbox on the same tick still reconciles.
func TestLivenessKeepsReconcilingWhenARestartWedges(t *testing.T) {
	r := &recorder{}
	wedged := optedIn()
	svc, l := newService(t, r, wedged, fastStart)
	l.provider.status = oomKilled()
	l.provider.wedgeStartOf = wedged.ID

	// A second OOM-killed sandbox that also asked for a restart; the wedge on the first must not starve it.
	other := optedIn()
	other.ID = "sandbox2"
	l.repo.made = &other

	var reports []string
	start := time.Now()
	err := svc.Liveness(t.Context(), []models.Sandbox{wedged, other}, time.Now(), func(line string) { reports = append(reports, line) })
	if err != nil {
		t.Fatalf("Liveness returned %v, want nil so the daemon task lives", err)
	}
	bounded(t, start, "the tick")

	// The wedged sandbox spent its restart and stays stopped; a raw start or rm is the operator's move now.
	if got := l.repo.sb; got.State != models.StateStopped || got.OOMRestarts != 1 {
		t.Errorf("the wedged sandbox is %s with %d starts again, want stopped with 1 counted", got.State, got.OOMRestarts)
	}
	// The other sandbox reconciled straight through the wedge: it started again and its record proves it.
	if got := *l.repo.made; got.State != models.StateRunning || got.PID != 7 || got.OOMRestarts != 1 {
		t.Errorf("the other sandbox is %s with pid %d and %d starts again, want running pid 7 with 1", got.State, got.PID, got.OOMRestarts)
	}
	if len(reports) != 2 {
		t.Fatalf("the tick reported %v, want one line per sandbox", reports)
	}
}

// A stop and a start that reused the PID during the tick changes StartedAt, so PID alone would miss the new
// run: the guard catches it on StartedAt and never applies the old run's status to the new one.
func TestLivenessBailsWhenAReusedPidHidesANewRun(t *testing.T) {
	r := &recorder{}
	old := running()
	old.StartedAt = time.Now().Add(-time.Minute)
	svc, l := newService(t, r, old, fastBudget)
	// The record now holds a fresh run behind the same PID 42: a later StartedAt.
	l.repo.sb.StartedAt = time.Now()

	if err := svc.Liveness(t.Context(), []models.Sandbox{old}, time.Now(), func(string) {}); err != nil {
		t.Fatalf("Liveness returned %v, want nil", err)
	}
	if slices.Contains(r.calls, "provider.Status") {
		t.Error("the tick probed a run the record had already replaced")
	}
	if got := l.repo.sb; got.State != models.StateRunning || got.PID != 42 {
		t.Errorf("the record is now %s pid %d, want the fresh run left running with pid 42", got.State, got.PID)
	}
}

// Startup reconcile is daemon-initiated, so a wedged Status must not hang boot: it leaves the record for the tick.
func TestReconcileLeavesTheRecordWhenTheSubstrateDoesNotAnswer(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running(), fastBudget)
	l.provider.statusGate = make(chan struct{})

	var reports []string
	start := time.Now()
	err := svc.ReconcileAll(t.Context(), []models.Sandbox{running()}, func(line string) { reports = append(reports, line) })
	if err != nil {
		t.Fatalf("ReconcileAll returned %v, want nil so a wedged substrate does not fail boot", err)
	}
	bounded(t, start, "reconcile")

	if len(reports) != 1 || !strings.Contains(reports[0], "did not answer within") {
		t.Errorf("reconcile reported %v, want one line on the wedged substrate", reports)
	}
	if got := l.repo.sb; got.State != models.StateRunning || got.PID != 42 {
		t.Errorf("the record is now %s pid %d, want it left running with pid 42", got.State, got.PID)
	}
}

// A substrate that answers the opening probe but never confirms the kill still returns inside the settle.
func TestStopIsBoundedWhenTheSubstrateNeverSettles(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running(), shortSettle)
	l.provider.aliveAfterStop = -1

	start := time.Now()
	_, err := svc.Stop(t.Context(), "sandbox1", sandbox.DefaultStopGrace)
	bounded(t, start, "stop")

	if err == nil || !strings.Contains(err.Error(), "did not stop within") {
		t.Fatalf("Stop returned %v, want a bounded settle timeout", err)
	}
	if !l.provider.stopped {
		t.Error("the kill was never attempted")
	}
}

// A plain stop of a running sandbox whose Status wedges fails fast and typed, not at the client timeout.
func TestStopFailsFastWhenTheSubstrateDoesNotAnswer(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running(), fastBudget)
	l.provider.statusGate = make(chan struct{})

	start := time.Now()
	_, err := svc.Stop(t.Context(), "sandbox1", sandbox.DefaultStopGrace)
	bounded(t, start, "stop")

	var timeout *sandbox.SubstrateTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("Stop returned %v, want a SubstrateTimeoutError", err)
	}
	if timeout.Op != "stop" {
		t.Errorf("the error names op %q, want stop", timeout.Op)
	}
	if l.provider.stopped || l.provider.reclaimed {
		t.Error("stop killed or reclaimed a running sandbox it could not read")
	}
}
