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
func (l *livenessLab) tick(t *testing.T, listed models.Sandbox) error {
	t.Helper()

	return l.tickAt(t, listed, time.Now().UTC())
}

// tickAt is a pass at the given clock, for what the OOM backoff waits on.
func (l *livenessLab) tickAt(t *testing.T, listed models.Sandbox, now time.Time) error {
	t.Helper()

	return l.svc.Liveness(t.Context(), []models.Sandbox{listed}, now, func(line string) { l.reports = append(l.reports, line) })
}

func TestLivenessRecordsAnEntrypointExitAndLeavesTheSandboxRunning(t *testing.T) {
	lab := newLivenessLab(t, running(), alive(42))
	lab.l.provider.entrypointExit = &models.ExitStatus{Code: 7}

	if err := lab.tick(t, running()); err != nil {
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
		_, err := lab.svc.Stop(t.Context(), "sandbox1")
		stopped <- err
	}()
	<-entered

	ticked := make(chan error, 1)
	go func() { ticked <- lab.tick(t, running()) }()
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

	if err := lab.tick(t, running()); err != nil {
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

	if err := lab.tick(t, sb); err != nil {
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
		if err := lab.tick(t, lab.l.repo.sb); err != nil {
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
	if err := lab.tick(t, lab.l.repo.sb); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if got := lab.l.repo.sb.ExitChannel; got != "" {
		t.Errorf("a good read left exit channel %q on the record", got)
	}
}

func TestLivenessStopsASandboxWhoseProcessDied(t *testing.T) {
	lab := newLivenessLab(t, running(), gone())

	if err := lab.tick(t, running()); err != nil {
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

	if err := lab.tick(t, sb); err != nil {
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

	if err := lab.tick(t, got); err != nil {
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

	if err := lab.tick(t, sb); err != nil {
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

	if err := lab.tick(t, unresponsive()); err != nil {
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

			if err := lab.tick(t, unresponsive()); err != nil {
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
	lab.l.repo.checkpointDir = dir

	if err := lab.tick(t, sb); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StatePaused || got.PID != 0 || got.Checkpoint != dir || got.Pausing {
		t.Errorf("the record is %s with pid %d, checkpoint %q and mark %v, want paused with pid 0, %s and no mark", got.State, got.PID, got.Checkpoint, got.Pausing, dir)
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
	lab.l.repo.checkpointDir = dir

	if err := lab.tick(t, sb); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	silent := lab.l.repo.sb
	if silent.State != models.StateUnresponsive || !silent.Pausing || silent.Checkpoint != "" {
		t.Fatalf("the record is %s with mark %v and checkpoint %q, want unresponsive with the mark kept and no pause: the shim may still answer", silent.State, silent.Pausing, silent.Checkpoint)
	}

	lab.l.provider.status = gone()
	if err := lab.tick(t, silent); err != nil {
		t.Fatalf("the second Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StatePaused || got.PID != 0 || got.Checkpoint != dir || got.Pausing || got.UnresponsiveReason != "" || got.StoppedReason != "" {
		t.Errorf("the record is %s with pid %d, checkpoint %q, mark %v and the reasons %q and %q; want paused with pid 0, %s, no mark and no reason", got.State, got.PID, got.Checkpoint, got.Pausing, got.UnresponsiveReason, got.StoppedReason, dir)
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
	lab.l.repo.checkpointDir = dir

	if err := lab.tick(t, sb); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	answered := lab.l.repo.sb
	if answered.State != models.StateRunning || answered.Pausing || answered.Checkpoint != "" {
		t.Fatalf("after the answer the record is %s with mark %v and checkpoint %q, want running with no mark and no pause", answered.State, answered.Pausing, answered.Checkpoint)
	}

	lab.l.provider.status = gone()
	if err := lab.tick(t, answered); err != nil {
		t.Fatalf("the second Liveness: %v", err)
	}
	if got := lab.l.repo.sb; got.State != models.StateStopped || got.Checkpoint != "" {
		t.Errorf("after the death the record is %s with checkpoint %q, want stopped with none: the checkpoint is older than the run", got.State, got.Checkpoint)
	}
}

// A shim that answers frozen beside its checkpoint ran nothing past the pause, so the mark stays.
func TestLivenessKeepsTheMarkWhenASilentShimAnswersFrozen(t *testing.T) {
	sb := unresponsive()
	sb.Pausing = true
	lab := newLivenessLab(t, sb, pausedAlive(sb.PID))

	if err := lab.tick(t, sb); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if got := lab.l.repo.sb; !got.Pausing {
		t.Errorf("the record is %s with no mark, want the mark kept: a frozen guest ran nothing past the pause", got.State)
	}
}

// A daemon cut after the swap leaves the sentry frozen beside a complete checkpoint, which the tick must release (SHARD-366).
func TestLivenessReleasesAMarkedSandboxItsPauseLeftFrozen(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sb := running()
	sb.Pausing = true
	lab := newLivenessLab(t, sb, frozen())
	lab.l.repo.checkpointDir = dir

	if err := lab.tick(t, sb); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StatePaused || got.PID != 0 || got.Checkpoint != dir || got.Pausing {
		t.Errorf("the record is %s with pid %d, checkpoint %q and mark %v, want paused with pid 0, %s and no mark", got.State, got.PID, got.Checkpoint, got.Pausing, dir)
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
	lab.l.repo.checkpointDir = dir

	if err := lab.tick(t, running()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	if got := lab.l.repo.sb; got.Checkpoint != "" {
		t.Errorf("the record took the checkpoint %q, want none: no pause marked it", got.Checkpoint)
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
	lab.l.repo.checkpointDir = t.TempDir()

	if err := lab.tick(t, sb); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	if slices.Contains(lab.r.snapshot(), "provider.Release") {
		t.Errorf("the calls were %v, want no release: no complete checkpoint stands beside the sentry", lab.r.snapshot())
	}
	if got := lab.l.repo.sb; got.Checkpoint != "" || !got.Pausing {
		t.Errorf("the record has checkpoint %q and mark %v, want no checkpoint and the mark", got.Checkpoint, got.Pausing)
	}
}

// A run the substrate carried on past its pause's checkpoint holds no pause, so a later death of it is no pause either (SHARD-429).
func TestLivenessDropsTheMarkOfASandboxTheSubstrateRunsPastItsCheckpoint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sb := running()
	sb.Pausing = true
	lab := newLivenessLab(t, sb, alive(42))
	lab.l.repo.checkpointDir = dir

	if err := lab.tick(t, sb); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if got := lab.l.repo.sb; got.State != models.StateRunning || got.Pausing {
		t.Errorf("the record is %s with mark %v, want running with no mark: the substrate runs it on", got.State, got.Pausing)
	}

	lab.l.provider.status = gone()
	if err := lab.tick(t, lab.l.repo.sb); err != nil {
		t.Fatalf("Liveness after the death: %v", err)
	}
	if got := lab.l.repo.sb; got.State != models.StateStopped || got.Checkpoint != "" {
		t.Errorf("the record is %s with checkpoint %q, want stopped with none: the run past the checkpoint died", got.State, got.Checkpoint)
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
	lab.l.repo.checkpointDir = dir

	if err := lab.tick(t, sb); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if got := lab.l.repo.sb; got.State != models.StateRunning || !got.Pausing {
		t.Errorf("the record is %s with mark %v, want running with the mark kept", got.State, got.Pausing)
	}

	lab.l.provider.status = gone()
	if err := lab.tick(t, lab.l.repo.sb); err != nil {
		t.Fatalf("Liveness after the death: %v", err)
	}
	if got := lab.l.repo.sb; got.State != models.StatePaused || got.Checkpoint != dir {
		t.Errorf("the record is %s with checkpoint %q, want paused with %s: nothing proved the run went past it", got.State, got.Checkpoint, dir)
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
	lab.l.repo.checkpointDir = dir
	// The second Status finds the sandbox gone into the checkpoint the first one predated.
	lab.l.provider.exits = func() {}

	if err := lab.tick(t, sb); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StatePaused || got.Checkpoint != dir || got.Pausing {
		t.Errorf("the record is %s with checkpoint %q and mark %v, want paused with %s and no mark", got.State, got.Checkpoint, got.Pausing, dir)
	}
}

// shard-init's own death is the sandbox exit, and its reason is what inspect shows (SHARD-290).
func TestLivenessRecordsTheExitAndTheReasonOfAShardInitThatDied(t *testing.T) {
	why := "supervisor: forward the stop to the entrypoint: operation not permitted"
	lab := newLivenessLab(t, running(), models.Status{Exists: true, State: models.StateStopped, SupervisorFailed: why})

	if err := lab.tick(t, running()); err != nil {
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

// The host ended it for its memory, so the record stops with the reason and the daemon owes it a start again (SHARD-786).
func TestLivenessStopsTheRecordAfterAnOOMAndOwesItAStart(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	sb := running()
	sb.Resources = models.Resources{MemoryMiB: 64}
	sb.RunStartedAt = now.Add(-time.Minute)
	// oomKilled is also what firecracker and vz report once shard-init's oom frame landed.
	lab := newLivenessLab(t, sb, oomKilled())

	if err := lab.tickAt(t, sb, now); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	got := lab.l.repo.sb
	if got.State != models.StateStopped || got.PID != 0 || got.StoppedReason != sandbox.OOMKilledReason {
		t.Errorf("the record says %s with pid %d and the reason %q, want stopped with %q", got.State, got.PID, got.StoppedReason, sandbox.OOMKilledReason)
	}
	want := models.OOM{Kills: 1, InARow: 1, KilledAt: now, RestartAt: now}
	if got.OOM == nil || *got.OOM != want {
		t.Errorf("the record holds the OOM %+v, want %+v", got.OOM, want)
	}
	// The start runs on the next tick, so this one never spends the start budget on top of its probes.
	if lab.l.provider.started || slices.Contains(lab.r.calls, "net.Allocate") {
		t.Errorf("the tick that saw the kill started the sandbox too: %v", lab.r.calls)
	}
	if len(lab.reports) != 1 || !strings.Contains(lab.reports[0], "the daemon starts it again now") {
		t.Errorf("the pass reported %v, want one line on the stop and the start again", lab.reports)
	}
}

// reconcile is a daemon start over an OOM it never saw, up to the first liveness tick.
func (l *livenessLab) reconcile(t *testing.T) {
	t.Helper()

	if err := l.svc.ReconcileAll(t.Context(), []models.Sandbox{l.l.repo.sb}, func(line string) { l.reports = append(l.reports, line) }, runOnce); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
}

// The boot stops the record, so no verb reads a running sandbox with no process, and the first tick starts it again (SHARD-311).
func TestReconcileStopsTheRecordOfAnOOMTheDaemonWasDownForAndTheFirstTickStartsIt(t *testing.T) {
	sb := running()
	sb.Resources = models.Resources{MemoryMiB: 64}
	lab := newLivenessLab(t, sb, oomKilled())

	lab.reconcile(t)

	got, err := sandbox.Inspect(lab.l.repo, &fakeEnforcer{}, "sandbox1")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got.State != models.StateStopped || got.PID != 0 || got.StoppedReason != sandbox.OOMKilledReason || !got.OOM.RestartDue() {
		t.Errorf("inspect says %s with pid %d, the reason %q and the OOM %+v, want stopped with %q and a start owed", got.State, got.PID, got.StoppedReason, got.OOM, sandbox.OOMKilledReason)
	}
	if last := lab.reports[len(lab.reports)-1]; !strings.Contains(last, "the record now says stopped") {
		t.Errorf("the reconcile reported %v, want a last line on the stop", lab.reports)
	}

	if err := lab.tick(t, lab.l.repo.sb); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if !lab.l.provider.started || lab.l.repo.sb.State != models.StateRunning {
		t.Errorf("after the first tick the record says %s, want running again: %v", lab.l.repo.sb.State, lab.r.calls)
	}
}

// The list may be a tick old, so a stop that landed since is read from the store before anything is asked.
func TestLivenessNeverTouchesASandboxTheRecordSaysStopped(t *testing.T) {
	sb := running()
	sb.State = models.StateStopped
	sb.PID = 0
	lab := newLivenessLab(t, sb, oomKilled())

	if err := lab.tick(t, running()); err != nil {
		t.Fatalf("Liveness: %v", err)
	}

	if lab.l.provider.started || slices.Contains(lab.r.calls, "provider.Status") {
		t.Errorf("a stopped sandbox reached the substrate: %v", lab.r.calls)
	}
}
