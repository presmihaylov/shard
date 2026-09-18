package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// recRepo holds the records a reconcile is given, and fails the test if one is ever deleted.
type recRepo struct {
	t       *testing.T
	records map[string]*models.Sandbox
}

func (r *recRepo) Get(id string) (models.Sandbox, error) {
	sb, ok := r.records[id]
	if !ok {
		return models.Sandbox{}, errors.New("no sandbox " + id)
	}

	return *sb, nil
}

func (r *recRepo) Resolve(ref string) (string, error) { return ref, nil }

func (r *recRepo) List() ([]models.Sandbox, error) {
	var out []models.Sandbox
	for _, sb := range r.records {
		out = append(out, *sb)
	}

	return out, nil
}

func (r *recRepo) Create(sb models.Sandbox) (models.Sandbox, error) { return sb, nil }

func (r *recRepo) Update(id string, mutate func(*models.Sandbox) error) error {
	sb, ok := r.records[id]
	if !ok {
		return errors.New("no sandbox " + id)
	}

	return mutate(sb)
}

func (r *recRepo) Delete(id string) error {
	r.t.Fatalf("a reconcile deleted the record of %s", id)

	return nil
}

func (r *recRepo) Dir(id string) (string, error)         { return "/state/" + id, nil }
func (r *recRepo) SnapshotDir(id string) (string, error) { return "/snapshots/" + id, nil }

// recProvider answers Status per id, which is the whole substrate a reconcile asks about.
type recProvider struct {
	models.Provider

	status map[string]models.Status
	err    error
	// wedge makes every Status block until the probe budget cancels it, the way a frozen sandbox does.
	wedge bool
}

func (p *recProvider) Name() string { return "fake" }

func (p *recProvider) Status(ctx context.Context, id string) (models.Status, error) {
	if p.wedge {
		<-ctx.Done()

		return models.Status{}, ctx.Err()
	}
	if p.err != nil {
		return models.Status{}, p.err
	}

	return p.status[id], nil
}

// recNet counts the re-applies, which is what the host netfilter rules cost after a restart.
type recNet struct {
	sandbox.Network

	applied int
	err     error
}

func (n *recNet) ReapplyAll(context.Context) error {
	n.applied++

	return n.err
}

type reconcileLab struct {
	svc     *sandbox.Service
	repo    *recRepo
	net     *recNet
	reports []string
}

func newReconcileLab(t *testing.T, provider *recProvider, records ...models.Sandbox) *reconcileLab {
	return newTunedReconcileLab(t, provider, 0, records...)
}

// newTunedReconcileLab is newReconcileLab with a probe budget; a zero budget keeps the default.
func newTunedReconcileLab(t *testing.T, provider *recProvider, budget time.Duration, records ...models.Sandbox) *reconcileLab {
	t.Helper()

	repo := &recRepo{t: t, records: map[string]*models.Sandbox{}}
	for _, sb := range records {
		repo.records[sb.ID] = &sb
	}

	lab := &reconcileLab{repo: repo, net: &recNet{}}
	lab.svc = sandbox.New(sandbox.Config{Repo: repo, Provider: provider, Network: lab.net, ProbeBudget: budget})

	return lab
}

func (l *reconcileLab) run(t *testing.T) error {
	t.Helper()

	records, err := l.repo.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	return l.svc.ReconcileAll(t.Context(), records, func(line string) { l.reports = append(l.reports, line) })
}

func alive(pid int) models.Status {
	return models.Status{Exists: true, State: models.StateRunning, PID: pid}
}

func gone() models.Status { return models.Status{} }

func TestReconcileStopsARunningRecordWithNoProcess(t *testing.T) {
	sb := models.Sandbox{ID: "sandbox1", State: models.StateRunning, PID: 42}
	lab := newReconcileLab(t, &recProvider{status: map[string]models.Status{"sandbox1": gone()}}, sb)

	if err := lab.run(t); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	got := lab.repo.records["sandbox1"]
	if got.State != models.StateStopped || got.PID != 0 {
		t.Errorf("the record says %s with pid %d, want stopped with no pid", got.State, got.PID)
	}
	if got.StoppedReason != sandbox.LostReason {
		t.Errorf("the record gives the reason %q, want %q", got.StoppedReason, sandbox.LostReason)
	}
	if len(lab.reports) != 1 || !strings.Contains(lab.reports[0], "sandbox1") {
		t.Errorf("the reconcile reported %v, want one line naming the sandbox", lab.reports)
	}
	if lab.net.applied != 0 {
		t.Errorf("the host rules were re-applied %d times, want none: nothing runs", lab.net.applied)
	}
}

func TestReconcileLeavesARunningSandboxAndReAppliesTheHostRules(t *testing.T) {
	sb := models.Sandbox{ID: "sandbox1", State: models.StateRunning, PID: 42}
	lab := newReconcileLab(t, &recProvider{status: map[string]models.Status{"sandbox1": alive(42)}}, sb)

	if err := lab.run(t); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	if got := lab.repo.records["sandbox1"]; got.State != models.StateRunning || got.StoppedReason != "" {
		t.Errorf("the record says %s with the reason %q, want running with none", got.State, got.StoppedReason)
	}
	if len(lab.reports) != 0 {
		t.Errorf("the reconcile reported %v for a record it corrected nothing on", lab.reports)
	}
	if lab.net.applied != 1 {
		t.Errorf("the host rules were re-applied %d times, want once", lab.net.applied)
	}
}

func TestReconcileCorrectsAStoppedRecordWithALiveProcess(t *testing.T) {
	sb := models.Sandbox{ID: "sandbox1", State: models.StateStopped, StoppedReason: sandbox.LostReason,
		ExitStatus: &models.ExitStatus{Code: 3}}
	lab := newReconcileLab(t, &recProvider{status: map[string]models.Status{"sandbox1": alive(99)}}, sb)

	if err := lab.run(t); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	got := lab.repo.records["sandbox1"]
	if got.State != models.StateRunning || got.PID != 99 {
		t.Errorf("the record says %s with pid %d, want running with pid 99", got.State, got.PID)
	}
	if got.StoppedReason != "" || got.ExitStatus != nil {
		t.Errorf("the record kept the reason %q and the exit %v of a run that ended", got.StoppedReason, got.ExitStatus)
	}
	if lab.net.applied != 1 {
		t.Errorf("the host rules were re-applied %d times, want once", lab.net.applied)
	}
}

func TestReconcileKeepsAPausedSandboxThatHoldsItsSnapshot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), []byte("snapshot"), 0o600); err != nil {
		t.Fatalf("write the checkpoint: %v", err)
	}

	sb := models.Sandbox{ID: "sandbox1", State: models.StatePaused, Snapshot: dir}
	lab := newReconcileLab(t, &recProvider{status: map[string]models.Status{"sandbox1": gone()}}, sb)

	if err := lab.run(t); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	if got := lab.repo.records["sandbox1"]; got.State != models.StatePaused {
		t.Errorf("the record says %s, want paused: the snapshot is what a paused sandbox has instead of a process", got.State)
	}
	if lab.net.applied != 0 {
		t.Errorf("the host rules were re-applied %d times, want none", lab.net.applied)
	}
}

func TestReconcileStopsAPausedRecordWhoseSnapshotIsGone(t *testing.T) {
	sb := models.Sandbox{ID: "sandbox1", State: models.StatePaused, Snapshot: filepath.Join(t.TempDir(), "empty")}
	lab := newReconcileLab(t, &recProvider{status: map[string]models.Status{"sandbox1": gone()}}, sb)

	if err := lab.run(t); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	got := lab.repo.records["sandbox1"]
	if got.State != models.StateStopped || got.StoppedReason != sandbox.LostReason {
		t.Errorf("the record says %s with the reason %q, want stopped with one", got.State, got.StoppedReason)
	}
}

func TestReconcileLeavesACreatedRecordAlone(t *testing.T) {
	sb := models.Sandbox{ID: "sandbox1", State: models.StateCreated}
	lab := newReconcileLab(t, &recProvider{status: map[string]models.Status{"sandbox1": gone()}}, sb)

	if err := lab.run(t); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	if got := lab.repo.records["sandbox1"]; got.State != models.StateCreated || got.StoppedReason != "" {
		t.Errorf("the record says %s with the reason %q, want created with none", got.State, got.StoppedReason)
	}
	if len(lab.reports) != 0 {
		t.Errorf("the reconcile reported %v for a record that never ran", lab.reports)
	}
}

func TestReconcileFailsAPendingRecordWithNoProcess(t *testing.T) {
	sb := models.Sandbox{ID: "sandbox1", State: models.StatePending, PID: 13}
	lab := newReconcileLab(t, &recProvider{status: map[string]models.Status{"sandbox1": gone()}}, sb)

	if err := lab.run(t); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	got := lab.repo.records["sandbox1"]
	if got.State != models.StateFailed || got.PID != 0 {
		t.Errorf("the record says %s with pid %d, want failed with no pid", got.State, got.PID)
	}
	if got.FailedReason != sandbox.InterruptedReason {
		t.Errorf("the record gives the reason %q, want %q", got.FailedReason, sandbox.InterruptedReason)
	}
	if len(lab.reports) != 1 || !strings.Contains(lab.reports[0], "sandbox1") {
		t.Errorf("the reconcile reported %v, want one line naming the sandbox", lab.reports)
	}
	if lab.net.applied != 0 {
		t.Errorf("the host rules were re-applied %d times, want none: nothing runs", lab.net.applied)
	}
}

func TestReconcileRunsAPendingRecordWithALiveProcess(t *testing.T) {
	sb := models.Sandbox{ID: "sandbox1", State: models.StatePending}
	lab := newReconcileLab(t, &recProvider{status: map[string]models.Status{"sandbox1": alive(51)}}, sb)

	if err := lab.run(t); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	got := lab.repo.records["sandbox1"]
	if got.State != models.StateRunning || got.PID != 51 {
		t.Errorf("the record says %s with pid %d, want running with pid 51: the start took before the daemon stopped", got.State, got.PID)
	}
	if got.FailedReason != "" {
		t.Errorf("the record kept the reason %q of a create that reached running", got.FailedReason)
	}
	if lab.net.applied != 1 {
		t.Errorf("the host rules were re-applied %d times, want once", lab.net.applied)
	}
}

func TestReconcileReportsEveryRecordItCorrected(t *testing.T) {
	status := map[string]models.Status{"sandbox1": gone(), "sandbox2": gone(), "sandbox3": alive(7)}
	lab := newReconcileLab(t, &recProvider{status: status},
		models.Sandbox{ID: "sandbox1", State: models.StateRunning},
		models.Sandbox{ID: "sandbox2", State: models.StateRunning},
		models.Sandbox{ID: "sandbox3", State: models.StateRunning})

	if err := lab.run(t); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	if len(lab.reports) != 2 {
		t.Errorf("the reconcile reported %v, want one line per corrected record", lab.reports)
	}
	if lab.net.applied != 1 {
		t.Errorf("the host rules were re-applied %d times, want once for the whole table", lab.net.applied)
	}
}

func TestReconcileAnswersWithWhatTheSubstrateRefused(t *testing.T) {
	provider := &recProvider{err: errors.New("runsc is not on this host")}
	lab := newReconcileLab(t, provider, models.Sandbox{ID: "sandbox1", State: models.StateRunning})

	err := lab.run(t)
	if err == nil || !strings.Contains(err.Error(), "runsc is not on this host") {
		t.Fatalf("ReconcileAll = %v, want the substrate's own refusal", err)
	}
	if got := lab.repo.records["sandbox1"]; got.State != models.StateRunning {
		t.Errorf("the record says %s, want it untouched while the substrate cannot answer", got.State)
	}
}

func TestReconcileAnswersWhenTheHostRulesCannotGoBackOn(t *testing.T) {
	lab := newReconcileLab(t, &recProvider{status: map[string]models.Status{"sandbox1": alive(42)}},
		models.Sandbox{ID: "sandbox1", State: models.StateRunning, PID: 42})
	lab.net.err = errors.New("nft is not on this host")

	err := lab.run(t)
	if err == nil || !strings.Contains(err.Error(), "nft is not on this host") {
		t.Fatalf("ReconcileAll = %v, want the failure of the re-apply", err)
	}
}

func TestReconcileProbesFrozenSandboxesConcurrently(t *testing.T) {
	const budget = 200 * time.Millisecond
	const frozen = 5

	records := make([]models.Sandbox, frozen)
	for i := range records {
		records[i] = models.Sandbox{ID: fmt.Sprintf("sandbox%d", i), State: models.StateRunning, PID: 42}
	}
	lab := newTunedReconcileLab(t, &recProvider{wedge: true}, budget, records...)

	start := time.Now()
	if err := lab.run(t); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	elapsed := time.Since(start)

	// Serial probing costs frozen*budget; concurrent costs about one budget, so half the serial cost still fails serial.
	if elapsed >= frozen*budget/2 {
		t.Errorf("the sweep took %s for %d frozen sandboxes at a %s budget, want it probed them concurrently", elapsed, frozen, budget)
	}
	if len(lab.reports) != frozen {
		t.Errorf("the reconcile reported %d lines, want one per timed-out sandbox", len(lab.reports))
	}
	for _, sb := range records {
		if got := lab.repo.records[sb.ID]; got.State != models.StateRunning || got.PID != 42 {
			t.Errorf("record %s says %s with pid %d, want it left running with pid 42 after a timed-out probe", sb.ID, got.State, got.PID)
		}
	}
	// A timed-out probe leaves the record running, so the host rules re-apply once for the running set.
	if lab.net.applied != 1 {
		t.Errorf("the host rules were re-applied %d times, want once for records left running", lab.net.applied)
	}
}

func TestReconcileRefusesWhenItCannotReadTheSnapshot(t *testing.T) {
	// A file where the snapshot directory belongs: the stat fails, and it fails with neither a yes nor a no.
	blocked := filepath.Join(t.TempDir(), "snapshot")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write the file in the way: %v", err)
	}

	sb := models.Sandbox{ID: "sandbox1", State: models.StatePaused, Snapshot: blocked}
	lab := newReconcileLab(t, &recProvider{status: map[string]models.Status{"sandbox1": gone()}}, sb)

	err := lab.run(t)
	if err == nil || !strings.Contains(err.Error(), blocked) {
		t.Fatalf("ReconcileAll = %v, want the stat that failed, naming %s", err, blocked)
	}

	got := lab.repo.records["sandbox1"]
	if got.State != models.StatePaused || got.StoppedReason != "" {
		t.Errorf("the record says %s with the reason %q: a stat that failed is not an absent checkpoint",
			got.State, got.StoppedReason)
	}
}
