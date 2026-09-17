package sandbox_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// A short budget so a wedged Status resolves fast, and a settle short enough to keep the test quick.
func fastBudget(c *sandbox.Config)  { c.ProbeBudget = 50 * time.Millisecond }
func shortSettle(c *sandbox.Config) { c.StopSettle = 100 * time.Millisecond }

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

// rm --force turns an unreadable sandbox into a kill, and frees everything once the kill lands.
func TestRemoveForceKillsAWedgedSandbox(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running(), fastBudget, shortSettle)
	l.provider.statusGate = make(chan struct{})
	l.provider.stopUnwedges = true

	start := time.Now()
	if err := svc.Remove(t.Context(), "sandbox1", true, sandbox.DefaultStopGrace); err != nil {
		t.Fatalf("Remove --force returned %v, want it to kill and free the wedged sandbox", err)
	}
	bounded(t, start, "rm --force")

	if !l.provider.stopped || !l.provider.removed || !l.repo.deleted {
		t.Errorf("rm --force left work undone: stopped=%v removed=%v deleted=%v", l.provider.stopped, l.provider.removed, l.repo.deleted)
	}
}

// A substrate that stays wedged past the kill still returns the stop inside the settle, never hanging.
func TestStopIsBoundedWhenTheSubstrateStaysWedged(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running(), shortSettle)
	l.provider.statusGate = make(chan struct{})

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
