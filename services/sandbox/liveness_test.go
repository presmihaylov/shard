package sandbox_test

import (
	"fmt"
	"os"
	"path/filepath"
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

// A stop on a guest that never answers holds its sandbox, and the pass must go on to the rest (SHARD-339).
func TestLivenessSkipsASandboxAVerbHolds(t *testing.T) {
	lab := newLivenessLab(t, running(), alive(42))
	lab.l.provider.entrypointExit = &models.ExitStatus{Code: 7}
	gate := make(chan struct{})
	lab.l.provider.stopGate = gate
	lab.l.provider.stopEntered = make(chan struct{})
	entered := lab.l.provider.stopEntered

	stopped := make(chan error, 1)
	go func() {
		_, err := lab.svc.Stop(t.Context(), "sandbox1", time.Second)
		stopped <- err
	}()
	<-entered

	ticked := make(chan error, 1)
	go func() { ticked <- lab.tick(t, running(), time.Now()) }()
	select {
	case err := <-ticked:
		if err != nil {
			t.Fatalf("Liveness: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the liveness pass waited on the sandbox the stop holds")
	}
	if lab.l.repo.sb.ExitStatus != nil {
		t.Errorf("the pass wrote exit %+v to the record the stop holds", lab.l.repo.sb.ExitStatus)
	}

	close(gate)
	if err := <-stopped; err != nil {
		t.Fatalf("stop: %v", err)
	}
	if n := lab.svc.Locks(); n != 0 {
		t.Errorf("%d sandbox locks outlived the stop and the pass that skipped it", n)
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

// Guest root can put its own file on PID 1's fd 0; inspect names it once, and a later good read clears it (SHARD-419).
func TestLivenessNamesAReplacedExitChannelOnceAndClearsIt(t *testing.T) {
	lab := newLivenessLab(t, running(), alive(42))
	lab.l.provider.entrypointErr = fmt.Errorf("fd 0 of PID 1 is not a regular file: %w", models.ErrExitChannelReplaced)

	for range 2 {
		if err := lab.tick(t, lab.l.repo.sb, time.Now()); err != nil {
			t.Fatalf("Liveness: %v", err)
		}
	}
	if got := lab.l.repo.sb; got.State != models.StateRunning || !strings.Contains(got.ExitChannel, "exit channel replaced") {
		t.Errorf("the record says %s with exit channel %q, want running and the channel named replaced", got.State, got.ExitChannel)
	}
	if len(lab.reports) != 1 {
		t.Errorf("two passes reported %v, want one line", lab.reports)
	}

	lab.l.provider.entrypointErr = nil
	if err := lab.tick(t, lab.l.repo.sb, time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if got := lab.l.repo.sb.ExitChannel; got != "" {
		t.Errorf("a good read left exit channel %q on the record", got)
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

// silentShim is what vz answers for a held shim that missed its probe bound (SHARD-421).
func silentShim() models.Status {
	return models.Status{Exists: true, State: models.StateUnresponsive, PID: 42, Reason: "its shim (pid 42) did not answer within 5s"}
}

// unresponsive is a running record that liveness marked for its silent shim.
func unresponsive() models.Sandbox {
	sb := running()
	sb.State = models.StateUnresponsive
	sb.UnresponsiveReason = silentShim().Reason

	return sb
}

func TestLivenessMarksASilentSandboxUnresponsiveAndNeverEndsIt(t *testing.T) {
	started := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	sb := running()
	sb.StartedAt = started
	lab := newLivenessLab(t, sb, silentShim())

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StateUnresponsive || got.PID != 42 || got.UnresponsiveReason != silentShim().Reason || !got.StartedAt.Equal(started) {
		t.Errorf("the record says %s with pid %d, the reason %q and the start %s; want unresponsive with its pid, the reason and its run", got.State, got.PID, got.UnresponsiveReason, got.StartedAt)
	}
	if lab.l.provider.stopped {
		t.Error("liveness stopped a sandbox that only went silent")
	}
	if len(lab.reports) != 1 || !strings.Contains(lab.reports[0], "pid 42") {
		t.Errorf("the pass reported %v, want one line naming the shim", lab.reports)
	}

	if err := lab.tick(t, got, time.Now()); err != nil {
		t.Fatalf("the second Liveness: %v", err)
	}
	if len(lab.reports) != 1 {
		t.Errorf("the second pass reported %v, want nothing new for a sandbox still silent", lab.reports[1:])
	}
}

func TestLivenessMakesAnUnresponsiveSandboxRunningOnceItAnswers(t *testing.T) {
	started := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	sb := unresponsive()
	sb.StartedAt = started
	lab := newLivenessLab(t, sb, alive(42))

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StateRunning || got.PID != 42 || got.UnresponsiveReason != "" || !got.StartedAt.Equal(started) {
		t.Errorf("the record says %s with pid %d, the reason %q and the start %s; want running with its run kept", got.State, got.PID, got.UnresponsiveReason, got.StartedAt)
	}
	if len(lab.reports) != 1 || !strings.Contains(lab.reports[0], "answers again") {
		t.Errorf("the pass reported %v, want one line on the answer", lab.reports)
	}
}

func TestLivenessStopsAnUnresponsiveSandboxWhoseProcessDied(t *testing.T) {
	lab := newLivenessLab(t, unresponsive(), gone())

	if err := lab.tick(t, unresponsive(), time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StateStopped || got.StoppedReason != sandbox.DiedReason || got.UnresponsiveReason != "" {
		t.Errorf("the record says %s with the reasons %q and %q, want stopped with %q alone", got.State, got.StoppedReason, got.UnresponsiveReason, sandbox.DiedReason)
	}
}

// An unresponsive sandbox the host ends for its memory, or whose shard-init dies, keeps only the reason it stopped for (SHARD-441).
func TestLivenessStopsAnUnresponsiveSandboxWithItsOwnReasonAlone(t *testing.T) {
	const why = "exec the entrypoint: no such file"
	for _, tc := range []struct {
		name   string
		status models.Status
		want   string
	}{
		{name: "the host ended it for its memory", status: oomKilled(), want: sandbox.OOMKilledReason},
		{name: "its shard-init died", status: models.Status{Exists: true, State: models.StateStopped, SupervisorFailed: why}, want: sandbox.SupervisorFailedReason + ": " + why},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lab := newLivenessLab(t, unresponsive(), tc.status)

			if err := lab.tick(t, unresponsive(), time.Now()); err != nil {
				t.Fatalf("Liveness: %v", err)
			}

			got := lab.l.repo.sb
			if got.State != models.StateStopped || got.StoppedReason != tc.want || got.UnresponsiveReason != "" {
				t.Errorf("the record says %s with the reasons %q and %q, want stopped with %q alone", got.State, got.StoppedReason, got.UnresponsiveReason, tc.want)
			}
		})
	}
}

// A pause whose own reconcile could not ask the substrate leaves its mark, and the tick must take the checkpoint it wrote (SHARD-366).
func TestLivenessPausesAMarkedRecordWhosePauseLeftACheckpoint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sb := running()
	sb.Pausing = true
	lab := newLivenessLab(t, sb, gone())
	lab.l.repo.snapshotDir = dir

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StatePaused || got.PID != 0 || got.Snapshot != dir || got.Pausing {
		t.Errorf("the record is %s with pid %d, snapshot %q and mark %v, want paused with pid 0, %s and no mark", got.State, got.PID, got.Snapshot, got.Pausing, dir)
	}
	if len(lab.reports) != 1 || !strings.Contains(lab.reports[0], "now says paused") {
		t.Errorf("the pass reported %v, want one line on the pause", lab.reports)
	}
}

// A daemon cut after the checkpoint leaves its mark over a frozen shim, and the tick must keep the pause through its silence and its death (SHARD-442).
func TestLivenessPausesAMarkedRecordWhoseAdoptedShimWentSilentAndThenDied(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sb := running()
	sb.Pausing = true
	lab := newLivenessLab(t, sb, silentShim())
	lab.l.repo.snapshotDir = dir

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	silent := lab.l.repo.sb
	if silent.State != models.StateUnresponsive || !silent.Pausing || silent.Snapshot != "" {
		t.Fatalf("the record is %s with mark %v and snapshot %q, want unresponsive with the mark kept and no pause: the shim may still answer", silent.State, silent.Pausing, silent.Snapshot)
	}

	lab.l.provider.status = gone()
	if err := lab.tick(t, silent, time.Now()); err != nil {
		t.Fatalf("the second Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StatePaused || got.PID != 0 || got.Snapshot != dir || got.Pausing || got.UnresponsiveReason != "" || got.StoppedReason != "" {
		t.Errorf("the record is %s with pid %d, snapshot %q, mark %v and the reasons %q and %q; want paused with pid 0, %s, no mark and no reason", got.State, got.PID, got.Snapshot, got.Pausing, got.UnresponsiveReason, got.StoppedReason, dir)
	}
	if len(lab.reports) != 2 || !strings.Contains(lab.reports[1], "said unresponsive") || !strings.Contains(lab.reports[1], "now says paused") {
		t.Errorf("the passes reported %v, want the silence and then the pause of an unresponsive record", lab.reports)
	}
}

// A silent shim that answers running again ran past the pause, so the tick drops the mark with the answer and its death stops the record (SHARD-442).
func TestLivenessDropsTheMarkWhenASilentShimAnswersRunningAgain(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sb := unresponsive()
	sb.Pausing = true
	lab := newLivenessLab(t, sb, alive(sb.PID))
	lab.l.repo.snapshotDir = dir

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	answered := lab.l.repo.sb
	if answered.State != models.StateRunning || answered.Pausing || answered.Snapshot != "" {
		t.Fatalf("after the answer the record is %s with mark %v and snapshot %q, want running with no mark and no pause", answered.State, answered.Pausing, answered.Snapshot)
	}

	lab.l.provider.status = gone()
	if err := lab.tick(t, answered, time.Now()); err != nil {
		t.Fatalf("the second Liveness: %v", err)
	}
	if got := lab.l.repo.sb; got.State != models.StateStopped || got.Snapshot != "" {
		t.Errorf("after the death the record is %s with snapshot %q, want stopped with none: the checkpoint is older than the run", got.State, got.Snapshot)
	}
}

// A shim that answers frozen beside its checkpoint ran nothing past the pause, so the mark stays.
func TestLivenessKeepsTheMarkWhenASilentShimAnswersFrozen(t *testing.T) {
	sb := unresponsive()
	sb.Pausing = true
	lab := newLivenessLab(t, sb, pausedAlive(sb.PID))

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if got := lab.l.repo.sb; !got.Pausing {
		t.Errorf("the record is %s with no mark, want the mark kept: a frozen guest ran nothing past the pause", got.State)
	}
}

// A daemon cut after the swap leaves the sentry frozen beside a complete snapshot, which the tick must release (SHARD-366).
func TestLivenessReleasesAMarkedSandboxItsPauseLeftFrozen(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sb := running()
	sb.Pausing = true
	lab := newLivenessLab(t, sb, frozen())
	lab.l.repo.snapshotDir = dir

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StatePaused || got.PID != 0 || got.Snapshot != dir || got.Pausing {
		t.Errorf("the record is %s with pid %d, snapshot %q and mark %v, want paused with pid 0, %s and no mark", got.State, got.PID, got.Snapshot, got.Pausing, dir)
	}
	if !slices.Contains(lab.r.snapshot(), "provider.Release") {
		t.Errorf("the calls were %v, want the frozen sandbox released: a resume refuses a live one", lab.r.snapshot())
	}
}

// Without the mark the frozen sentry is no pause this daemon finishes, so the tick neither releases it nor takes the checkpoint.
func TestLivenessLeavesAnUnmarkedFrozenSandboxAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lab := newLivenessLab(t, running(), frozen())
	lab.l.repo.snapshotDir = dir

	if err := lab.tick(t, running(), time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	if got := lab.l.repo.sb; got.Snapshot != "" {
		t.Errorf("the record took the snapshot %q, want none: no pause marked it", got.Snapshot)
	}
	if slices.Contains(lab.r.snapshot(), "provider.Release") {
		t.Errorf("the calls were %v, want no release of a sentry no marked pause left", lab.r.snapshot())
	}
}

// A pause cut before its checkpoint was complete finished nothing, so the tick leaves the frozen sentry alone.
func TestLivenessReleasesNoMarkedSandboxWhosePauseLeftNoCompleteCheckpoint(t *testing.T) {
	sb := running()
	sb.Pausing = true
	lab := newLivenessLab(t, sb, frozen())
	lab.l.repo.snapshotDir = t.TempDir()

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	if slices.Contains(lab.r.snapshot(), "provider.Release") {
		t.Errorf("the calls were %v, want no release: no complete checkpoint stands beside the sentry", lab.r.snapshot())
	}
	if got := lab.l.repo.sb; got.Snapshot != "" || !got.Pausing {
		t.Errorf("the record has snapshot %q and mark %v, want no snapshot and the mark", got.Snapshot, got.Pausing)
	}
}

// A run the substrate carried on past its pause's snapshot holds no pause, so a later death of it is no pause either (SHARD-429).
func TestLivenessDropsTheMarkOfASandboxTheSubstrateRunsPastItsSnapshot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sb := running()
	sb.Pausing = true
	lab := newLivenessLab(t, sb, alive(42))
	lab.l.repo.snapshotDir = dir

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if got := lab.l.repo.sb; got.State != models.StateRunning || got.Pausing {
		t.Errorf("the record is %s with mark %v, want running with no mark: the substrate runs it on", got.State, got.Pausing)
	}

	lab.l.provider.status = gone()
	if err := lab.tick(t, lab.l.repo.sb, time.Now()); err != nil {
		t.Fatalf("Liveness after the death: %v", err)
	}
	if got := lab.l.repo.sb; got.State != models.StateStopped || got.Snapshot != "" {
		t.Errorf("the record is %s with snapshot %q, want stopped with none: the run past the snapshot died", got.State, got.Snapshot)
	}
}

func TestLivenessKeepsTheMarkOfASandboxTheSubstrateDoesNotSayRuns(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sb := running()
	sb.Pausing = true
	lab := newLivenessLab(t, sb, unproven(42))
	lab.l.repo.snapshotDir = dir

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if got := lab.l.repo.sb; got.State != models.StateRunning || !got.Pausing {
		t.Errorf("the record is %s with mark %v, want running with the mark kept", got.State, got.Pausing)
	}

	lab.l.provider.status = gone()
	if err := lab.tick(t, lab.l.repo.sb, time.Now()); err != nil {
		t.Fatalf("Liveness after the death: %v", err)
	}
	if got := lab.l.repo.sb; got.State != models.StatePaused || got.Snapshot != dir {
		t.Errorf("the record is %s with snapshot %q, want paused with %s: nothing proved the run went past it", got.State, got.Snapshot, dir)
	}
}

// The tick probes before it takes the lock, so a pause can commit after the probe; only a probe under the lock drops the mark.
func TestLivenessKeepsThePauseThatCommittedAfterItsProbe(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sb := running()
	sb.Pausing = true
	lab := newLivenessLab(t, sb, alive(42))
	lab.l.repo.snapshotDir = dir
	// The second Status finds the sandbox gone into the snapshot the first one predated.
	lab.l.provider.exits = func() {}

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StatePaused || got.Snapshot != dir || got.Pausing {
		t.Errorf("the record is %s with snapshot %q and mark %v, want paused with %s and no mark", got.State, got.Snapshot, got.Pausing, dir)
	}
}

// shard-init's own death is the sandbox exit, and its reason is what inspect shows (SHARD-290).
func TestLivenessRecordsTheExitAndTheReasonOfAShardInitThatDied(t *testing.T) {
	why := "supervisor: forward the stop to the entrypoint: operation not permitted"
	lab := newLivenessLab(t, running(), models.Status{Exists: true, State: models.StateStopped, SupervisorFailed: why})

	if err := lab.tick(t, running(), time.Now()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	want := sandbox.SupervisorFailedReason + ": " + why
	if got.State != models.StateStopped || got.PID != 0 || got.StoppedReason != want {
		t.Errorf("the record says %s with pid %d and the reason %q, want stopped with %q", got.State, got.PID, got.StoppedReason, want)
	}
	if got.ExitStatus == nil || *got.ExitStatus != (models.ExitStatus{Code: models.SupervisorFailedExitCode}) {
		t.Errorf("the record holds the exit %+v, want the supervisor's %d", got.ExitStatus, models.SupervisorFailedExitCode)
	}
	if len(lab.reports) != 1 || !strings.Contains(lab.reports[0], why) {
		t.Errorf("the pass reported %v, want one line with the reason", lab.reports)
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

// reconcile is a daemon start over an OOM it never saw, up to the first liveness tick.
func (l *livenessLab) reconcile(t *testing.T) {
	t.Helper()

	if err := l.svc.ReconcileAll(t.Context(), []models.Sandbox{l.l.repo.sb}, func(line string) { l.reports = append(l.reports, line) }, runOnce); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
}

// The start again happens at boot, and the first tick then finds a live sandbox, not a second OOM (SHARD-311).
func TestReconcileStartsAfterAnOOMTheDaemonWasDownFor(t *testing.T) {
	sb := optedIn()
	sb.MaxOOMRestarts = 3
	lab := newLivenessLab(t, sb, oomKilled())

	before := time.Now().UTC()
	lab.reconcile(t)
	after := time.Now().UTC()

	got := lab.l.repo.sb
	if got.State != models.StateRunning || got.PID != 7 || got.StoppedReason != "" {
		t.Errorf("the record says %s with pid %d and the reason %q, want running with the new pid", got.State, got.PID, got.StoppedReason)
	}
	if got.OOMRestarts != 1 || got.OOMRestartedAt.Before(before) || got.OOMRestartedAt.After(after) {
		t.Errorf("the record counts %d starts again at %v, want 1 within the reconcile", got.OOMRestarts, got.OOMRestartedAt)
	}
	if last := lab.reports[len(lab.reports)-1]; !strings.Contains(last, "started again, 1 of 3") {
		t.Errorf("the reconcile reported %v, want a last line counting the start", lab.reports)
	}

	if err := lab.tick(t, lab.l.repo.sb, time.Now().UTC()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if got := lab.l.repo.sb; got.State != models.StateRunning || got.OOMRestarts != 1 {
		t.Errorf("after the first tick the record says %s with %d starts again, want running with 1", got.State, got.OOMRestarts)
	}
}

// The tick's backoff bounds a loop of ticks; at boot it would leave a running record with no process behind it.
func TestReconcileStartsAfterAnOOMInsideTheTickBackoff(t *testing.T) {
	now := time.Now().UTC()
	sb := optedIn()
	sb.MaxOOMRestarts = 5
	sb.OOMRestarts = 3
	sb.OOMRestartedAt = now
	sb.StartedAt = now
	lab := newLivenessLab(t, sb, oomKilled())

	lab.reconcile(t)

	if got := lab.l.repo.sb; got.State != models.StateRunning || got.PID != 7 || got.OOMRestarts != 4 {
		t.Errorf("the record says %s with pid %d and %d starts again, want running with pid 7 and 4", got.State, got.PID, got.OOMRestarts)
	}
}

// Every verb that reads a record between the boot and the first tick reads the memory decision, never the dead pid.
func TestInspectBeforeTheFirstTickReadsTheOOMTheDaemonWasDownFor(t *testing.T) {
	cases := []struct {
		name   string
		sb     func() models.Sandbox
		fail   []string
		state  models.State
		pid    int
		reason string
		report string
	}{
		{"no restart_on_oom", running, nil, models.StateStopped, 0, sandbox.OOMKilledReason, "the record now says stopped"},
		{"restart_on_oom", optedIn, nil, models.StateRunning, 7, "", "started again, 1"},
		{"a start again that fails", optedIn, []string{"provider.Start"}, models.StateStopped, 0, sandbox.OOMKilledReason, "the record now says stopped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lab := newLivenessLab(t, tc.sb(), oomKilled())
			lab.r.fail = tc.fail

			lab.reconcile(t)

			got, err := sandbox.Inspect(lab.l.repo, &fakeEnforcer{}, "sandbox1")
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if got.State != tc.state || got.PID != tc.pid || got.StoppedReason != tc.reason {
				t.Errorf("inspect says %s with pid %d and the reason %q, want %s with pid %d and %q", got.State, got.PID, got.StoppedReason, tc.state, tc.pid, tc.reason)
			}
			if last := lab.reports[len(lab.reports)-1]; !strings.Contains(last, tc.report) {
				t.Errorf("the reconcile reported %v, want a last line with %q", lab.reports, tc.report)
			}
		})
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

// throttled is a live sandbox the host has held at its memory throttle the given number of times.
func throttled(count int64) models.Status {
	status := alive(42)
	status.Throttles = count

	return status
}

// A gvisor OOM loop sits at memory.high for longer than the reset window, and the cap must still spend (SHARD-332).
func TestLivenessSpendsTheOOMLimitOfARunHeldAtTheThrottle(t *testing.T) {
	start := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	sb := optedIn()
	sb.MaxOOMRestarts = 2
	sb.OOMRestarts = 2
	sb.StartedAt = start
	sb.OOMRestartedAt = start.Add(-time.Hour)
	lab := newLivenessLab(t, sb, throttled(0))

	for i, count := range []int64{40, 90, 160, 230, 310, 380} {
		lab.l.provider.status = throttled(count)
		if err := lab.tick(t, sb, start.Add(time.Duration(i+1)*5*time.Second)); err != nil {
			t.Fatalf("Liveness: %v", err)
		}
	}
	if got := lab.l.repo.sb; got.HealthyRun || got.MemoryThrottles != 380 || !got.CalmSince.Equal(start.Add(30*time.Second)) {
		t.Fatalf("the record says healthy %t at %d throttles calm since %v, want unhealthy at 380 since the last tick", got.HealthyRun, got.MemoryThrottles, got.CalmSince)
	}

	oom := oomKilled()
	oom.Throttles = 420
	lab.l.provider.status = oom
	if err := lab.tick(t, sb, start.Add(35*time.Second)); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StateStopped || !strings.Contains(got.StoppedReason, "the 2 starts again the limit allows are spent") {
		t.Errorf("the record says %s with the reason %q, want stopped with the limit spent", got.State, got.StoppedReason)
	}
	if lab.l.provider.started {
		t.Error("a run held at the throttle for 35 s reset its count and was started again")
	}
}

// A run seen ten seconds under the throttle latches healthy, so the throttled OOM that ends it still resets the count.
func TestLivenessResetsTheOOMCountAfterACalmRunUnderTheThrottle(t *testing.T) {
	start := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	sb := optedIn()
	sb.MaxOOMRestarts = 2
	sb.OOMRestarts = 2
	sb.StartedAt = start
	sb.OOMRestartedAt = start.Add(-time.Hour)
	lab := newLivenessLab(t, sb, throttled(0))

	for i, count := range []int64{0, 0, 70, 300} {
		lab.l.provider.status = throttled(count)
		if err := lab.tick(t, sb, start.Add(time.Duration(i+1)*5*time.Second)); err != nil {
			t.Fatalf("Liveness: %v", err)
		}
	}
	if got := lab.l.repo.sb; !got.HealthyRun || got.MemoryThrottles != 300 {
		t.Fatalf("the record says healthy %t at %d throttles, want the calm 10 s latched through the throttle", got.HealthyRun, got.MemoryThrottles)
	}

	oom := oomKilled()
	oom.Throttles = 900
	lab.l.provider.status = oom
	if err := lab.tick(t, sb, start.Add(40*time.Second)); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StateRunning || got.OOMRestarts != 1 {
		t.Errorf("the record says %s with %d starts again, want running with the count reset then 1", got.State, got.OOMRestarts)
	}
	// The start again is a new run, which has to earn its own calm.
	if got.HealthyRun || got.MemoryThrottles != 0 || !got.CalmSince.IsZero() {
		t.Errorf("the new run kept healthy %t, %d throttles, calm since %v, want all three cleared", got.HealthyRun, got.MemoryThrottles, got.CalmSince)
	}
	if len(lab.reports) != 1 || !strings.Contains(lab.reports[0], "started again, 1 of 2") {
		t.Errorf("the pass reported %v, want the count reset to 1 of 2", lab.reports)
	}
}

// Only a sandbox that asked for OOM restarts needs the count, so no other record is written on every tick.
func TestLivenessKeepsNoThrottleCountWithoutRestartOnOOM(t *testing.T) {
	sb := running()
	sb.StartedAt = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	lab := newLivenessLab(t, sb, throttled(500))

	if err := lab.tick(t, sb, sb.StartedAt.Add(time.Minute)); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	if got := lab.l.repo.sb; got.MemoryThrottles != 0 || got.HealthyRun || !got.CalmSince.IsZero() {
		t.Errorf("a sandbox with no OOM restart kept %d throttles, healthy %t, calm since %v", got.MemoryThrottles, got.HealthyRun, got.CalmSince)
	}
}

// The wait takes the record out of running at the kill, so inspect never names the dead pid (SHARD-425).
func TestLivenessStopsTheRecordWhileTheOOMBackoffWaits(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	due := now.Add(time.Second)
	sb := optedIn()
	sb.OOMRestarts = 2
	sb.OOMRestartedAt = now.Add(-time.Second)
	lab := newLivenessLab(t, sb, alive(42))
	runExecToItsEnd(t, lab.svc)
	lab.l.provider.status = oomKilled()

	// Two starts again put the wait at 2 s, and only one has passed.
	if err := lab.tick(t, sb, now); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if held := lab.svc.ExecsHeld("sandbox1"); held != 0 {
		t.Errorf("the daemon holds %d execs of the killed sandbox through the wait, want none (SHARD-362)", held)
	}
	got, err := sandbox.Inspect(lab.l.repo, &fakeEnforcer{}, "sandbox1")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got.State != models.StateStopped || got.PID != 0 || !strings.Contains(got.StoppedReason, "it starts again at 2026-09-16T12:00:01Z") {
		t.Errorf("inspect in the wait reads %s with pid %d and the reason %q, want stopped with no pid and the wait named", got.State, got.PID, got.StoppedReason)
	}
	if !got.OOMRestartDue.Equal(due) || got.OOMRestarts != 2 || lab.l.provider.started {
		t.Errorf("the record waits until %v with %d starts again (started %t), want %v with 2 and no start", got.OOMRestartDue, got.OOMRestarts, lab.l.provider.started, due)
	}

	// The next tick lists the record the wait wrote, and leaves it alone before the due time.
	if err := lab.tick(t, lab.l.repo.sb, due.Add(-500*time.Millisecond)); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if lab.l.provider.started {
		t.Error("the sandbox started again before its wait passed")
	}

	if err := lab.tick(t, lab.l.repo.sb, due); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	after := lab.l.repo.sb
	if after.State != models.StateRunning || after.PID != 7 || after.StoppedReason != "" || !after.OOMRestartDue.IsZero() {
		t.Errorf("once the wait passed the record says %s with pid %d, the reason %q and due %v, want running with the new pid", after.State, after.PID, after.StoppedReason, after.OOMRestartDue)
	}
	if after.OOMRestarts != 3 || !after.OOMRestartedAt.Equal(due) {
		t.Errorf("the record counts %d starts again at %v, want 3 at %v", after.OOMRestarts, after.OOMRestartedAt, due)
	}
	if len(lab.reports) != 2 || !strings.Contains(lab.reports[1], "started again, 3") {
		t.Errorf("the passes reported %v, want the wait and then the start counted", lab.reports)
	}
}

// waiting is a record the tick took out of running at an OOM kill, with its start again due at due.
func waiting(due time.Time) models.Sandbox {
	sb := optedIn()
	sb.State = models.StateStopped
	sb.PID = 0
	sb.StoppedReason = sandbox.OOMKilledReason + "; it starts again at " + due.Format(time.RFC3339)
	sb.OOMRestarts = 2
	sb.OOMRestartedAt = due.Add(-2 * time.Second)
	sb.OOMRestartDue = due

	return sb
}

// A stop was final in the wait while the record said running, so it stays final now that it says stopped.
func TestStopCallsOffTheStartAgainTheOOMBackoffWaits(t *testing.T) {
	due := time.Date(2026, 9, 16, 12, 0, 1, 0, time.UTC)
	lab := newLivenessLab(t, waiting(due), oomKilled())

	if _, err := lab.svc.Stop(t.Context(), "sandbox1", time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got := lab.l.repo.sb
	if got.State != models.StateStopped || got.StoppedReason != sandbox.OOMKilledReason || !got.OOMRestartDue.IsZero() {
		t.Errorf("after the stop the record says %s with the reason %q and due %v, want stopped with %q and nothing due", got.State, got.StoppedReason, got.OOMRestartDue, sandbox.OOMKilledReason)
	}

	if err := lab.tick(t, waiting(due), due.Add(time.Minute)); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if lab.l.provider.started {
		t.Error("the daemon started a sandbox the operator stopped in the wait")
	}
}

func TestStartByHandInTheOOMBackoffTakesTheWaitAway(t *testing.T) {
	due := time.Date(2026, 9, 16, 12, 0, 1, 0, time.UTC)
	lab := newLivenessLab(t, waiting(due), oomKilled())

	if _, err := lab.svc.Start(t.Context(), "sandbox1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := lab.l.repo.sb; got.State != models.StateRunning || !got.OOMRestartDue.IsZero() || got.OOMRestarts != 2 {
		t.Errorf("after the start the record says %s with due %v and %d starts again, want running with nothing due and 2", got.State, got.OOMRestartDue, got.OOMRestarts)
	}

	if err := lab.tick(t, waiting(due), due); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if starts := keep(lab.r.calls, "provider.Start"); len(starts) != 1 {
		t.Errorf("the sandbox was started %d times, want only the start by hand", len(starts))
	}
}

func TestLivenessSkipsAWaitAnRmTookAway(t *testing.T) {
	due := time.Date(2026, 9, 16, 12, 0, 1, 0, time.UTC)
	lab := newLivenessLab(t, waiting(due), oomKilled())
	lab.l.repo.missing = true

	if err := lab.tick(t, waiting(due), due); err != nil {
		t.Errorf("Liveness over a removed record = %v, want nil", err)
	}
	if lab.l.provider.started {
		t.Error("the daemon started a sandbox rm removed")
	}
}

// The record counts the start before it runs, so one that fails at the end of the wait stays stopped and leaves no loop.
func TestLivenessCountsAStartAgainThatFailedAfterTheWait(t *testing.T) {
	due := time.Date(2026, 9, 16, 12, 0, 1, 0, time.UTC)
	lab := newLivenessLab(t, waiting(due), oomKilled())
	lab.r.fail = []string{"provider.Start"}

	err := lab.tick(t, waiting(due), due)
	if err == nil || !strings.Contains(err.Error(), "start sandbox sandbox1 again") {
		t.Fatalf("Liveness = %v, want the start's failure", err)
	}
	got := lab.l.repo.sb
	if got.State != models.StateStopped || got.OOMRestarts != 3 || got.StoppedReason != sandbox.OOMKilledReason || !got.OOMRestartDue.IsZero() {
		t.Errorf("the record says %s with %d starts again, the reason %q and due %v, want stopped with 3, %q and nothing due", got.State, got.OOMRestarts, got.StoppedReason, got.OOMRestartDue, sandbox.OOMKilledReason)
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
