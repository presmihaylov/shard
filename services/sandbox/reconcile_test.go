package sandbox_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
}

func (p *recProvider) Name() string { return "fake" }

func (p *recProvider) Status(_ context.Context, id string) (models.Status, error) {
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
	t.Helper()

	repo := &recRepo{t: t, records: map[string]*models.Sandbox{}}
	for _, sb := range records {
		repo.records[sb.ID] = &sb
	}

	lab := &reconcileLab{repo: repo, net: &recNet{}}
	lab.svc = sandbox.New(sandbox.Config{Repo: repo, Provider: provider, Network: lab.net})

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
