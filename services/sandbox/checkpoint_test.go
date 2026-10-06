package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

func TestPauseWritesTheCheckpointAndRecordsIt(t *testing.T) {
	r := &recorder{}
	source := running()
	source.Name = "web"
	source.ExitStatus = &models.ExitStatus{Code: 3}
	svc, l := newService(t, r, source)

	sb, err := svc.Pause(t.Context(), "web")
	if err != nil {
		t.Fatalf("pause: %v", err)
	}

	if sb.ID != "sandbox1" || sb.State != models.StatePaused || sb.PID != 0 || sb.Checkpoint != "/checkpoints/sandbox1" {
		t.Errorf("pause answered %+v, want sandbox1 paused with pid 0 and its checkpoint", sb)
	}
	if l.provider.checkpointDir != "/checkpoints/sandbox1" {
		t.Errorf("the provider was told to write %q, want the repository's checkpoint directory", l.provider.checkpointDir)
	}
	// The entrypoint's exit is part of the run the checkpoint froze.
	if sb.ExitStatus == nil || sb.ExitStatus.Code != 3 {
		t.Errorf("the record lost its exit: %+v", sb.ExitStatus)
	}

	// The mark goes on before the provider writes, and comes off with the paused state after it.
	if got := keep(r.calls, "provider.Pause", "repo.Update"); !slices.Equal(got, []string{"repo.Update", "provider.Pause", "repo.Update"}) {
		t.Errorf("the pause and the record ran as %v, want the mark, the pause, then the record", got)
	}
	if sb.Pausing {
		t.Error("the paused record kept the mark of its pause")
	}
}

func TestPauseRefusesASandboxThatIsNotRunning(t *testing.T) {
	for _, state := range []models.State{models.StateStopped, models.StateCreated, models.StatePaused} {
		sb := running()
		sb.State = state
		svc, l := newService(t, &recorder{}, sb)

		_, err := svc.Pause(t.Context(), "sandbox1")
		if err == nil || !strings.Contains(err.Error(), string(state)) {
			t.Errorf("pause of a %s sandbox returned %v, want a refusal that names the state", state, err)
		}
		if l.provider.paused {
			t.Errorf("pause of a %s sandbox reached the provider", state)
		}
	}
}

// A pause into a silent process records it at once, and spends the one probe bound the provider already spent (SHARD-424).
func TestPauseIntoASilentProcessRecordsItUnresponsiveWithoutAskingAgain(t *testing.T) {
	r := &recorder{}
	var reports []string
	svc, l := newService(t, r, running(), func(cfg *sandbox.Config) {
		cfg.Report = func(line string) { reports = append(reports, line) }
	})
	l.provider.status = silentShim()
	l.provider.pauseErr = &models.UnresponsiveError{Sandbox: "sandbox1", Provider: "fake", Verb: models.VerbPause, Reason: silentShim().Reason}

	_, err := svc.Pause(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "is unresponsive: "+silentShim().Reason) {
		t.Errorf("pause into a silent process returned %v, want the refusal with the reason", err)
	}
	if slices.Contains(r.calls, "provider.Status") {
		t.Errorf("pause asked the substrate again after its refusal: %v", r.calls)
	}

	sb := l.repo.sb
	if sb.State != models.StateUnresponsive || sb.UnresponsiveReason != silentShim().Reason || sb.PID != running().PID {
		t.Errorf("the record is %s with the reason %q and pid %d, want unresponsive with the reason and its pid", sb.State, sb.UnresponsiveReason, sb.PID)
	}
	if len(reports) != 1 || !strings.Contains(reports[0], "pid 42") {
		t.Errorf("the pause reported %v, want one line naming the shim", reports)
	}
}

// An unresponsive record refuses a pause with the reason it holds, without reaching the provider.
func TestPauseRefusesAnUnresponsiveSandboxWithItsReason(t *testing.T) {
	svc, l := newService(t, &recorder{}, unresponsive())

	_, err := svc.Pause(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "is unresponsive: "+silentShim().Reason+": pause takes a running sandbox") {
		t.Errorf("pause of an unresponsive sandbox returned %v, want the refusal with the reason", err)
	}
	if l.provider.paused {
		t.Error("pause of an unresponsive sandbox reached the provider")
	}
}

func TestPauseKeepsTheRecordRunningWhenTheSandboxStillIs(t *testing.T) {
	svc, l := newService(t, &recorder{fail: []string{"provider.Pause"}}, running())

	if _, err := svc.Pause(t.Context(), "sandbox1"); err == nil {
		t.Fatal("pause returned no error")
	}

	if sb := l.repo.sb; sb.State != models.StateRunning || sb.Checkpoint != "" || sb.Pausing {
		t.Errorf("the record is %s with checkpoint %q and mark %v after a failed pause, want running with neither", sb.State, sb.Checkpoint, sb.Pausing)
	}
}

// A checkpoint an earlier pause and resume left must not pass for a pause that failed after the guest died (SHARD-366).
func TestAFailedPauseNeverTakesTheCheckpointAnEarlierPauseLeft(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "checkpoint.img")
	if err := os.WriteFile(stale, []byte("an earlier pause"), 0o600); err != nil {
		t.Fatal(err)
	}

	resumed := running()
	resumed.Checkpoint = dir
	svc, l := newService(t, &recorder{fail: []string{"provider.Pause"}}, resumed)
	l.repo.checkpointDir = dir
	l.provider.status = models.Status{}

	_, err := svc.Pause(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("pause returned %v, want the failure and that the sandbox is gone", err)
	}

	if sb := l.repo.sb; sb.State != models.StateStopped || sb.Pausing {
		t.Errorf("the record is %s with mark %v, want stopped with none: a resume would restore the older run", sb.State, sb.Pausing)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old checkpoint is still there: %v", err)
	}
}

// The old checkpoint goes before the mark, so a mark never stands over a checkpoint an earlier pause wrote.
func TestPauseRemovesTheOldCheckpointBeforeItMarksTheRecord(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "checkpoint.img")
	if err := os.WriteFile(stale, []byte("an earlier pause"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &recorder{fail: []string{"repo.Update#1"}}
	svc, l := newService(t, r, running())
	l.repo.checkpointDir = dir

	_, err := svc.Pause(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "mark the pause") {
		t.Fatalf("pause returned %v, want the mark that failed", err)
	}

	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the mark was written before the old checkpoint went: %v", err)
	}
	if slices.Contains(r.calls, "provider.Pause") {
		t.Errorf("a pause with no mark reached the provider: %v", r.calls)
	}
}

// A pause that lost the sandbox on the way must say so, or rm refuses a record that says running.
func TestPauseRecordsASandboxTheProviderLost(t *testing.T) {
	svc, l := newService(t, &recorder{fail: []string{"provider.Pause"}}, running())
	l.provider.status = models.Status{}

	_, err := svc.Pause(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("pause returned %v, want the failure and that the sandbox is gone", err)
	}

	if sb := l.repo.sb; sb.State != models.StateStopped || sb.PID != 0 {
		t.Errorf("the record is %s with pid %d, want stopped with pid 0", sb.State, sb.PID)
	}
}

// A client that hangs up mid-checkpoint must not cut the save, because the guest does not survive one that broke off.
func TestPauseOutlivesTheClientThatAskedForIt(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	sb, err := svc.Pause(ctx, "sandbox1")
	if err != nil {
		t.Fatalf("pause: %v", err)
	}

	if l.provider.pauseCtxErr != nil {
		t.Errorf("the provider paused under a context that said %v, want one the client cannot cancel", l.provider.pauseCtxErr)
	}
	if sb.State != models.StatePaused {
		t.Errorf("the record is %s, want paused", sb.State)
	}
}

// A pause that lost the guest leaves nothing to stop or resume, so the record ends failed with the reason.
func TestPauseThatLostTheGuestEndsTheRecordFailed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	svc, l := newService(t, &recorder{}, running())
	l.repo.checkpointDir = dir
	l.provider.lose = true

	_, err := svc.Pause(t.Context(), "sandbox1")
	var lost *models.LostError
	if !errors.As(err, &lost) {
		t.Fatalf("pause returned %v, want the lost sandbox", err)
	}

	// The checkpoint an earlier pause left must not pass for this one.
	sb := l.repo.sb
	if sb.State != models.StateFailed || sb.PID != 0 || !strings.Contains(sb.FailedReason, "no space left on device") || sb.Pausing {
		t.Errorf("the record is %s with pid %d, reason %q and mark %v, want failed with pid 0, the checkpoint's reason and no mark", sb.State, sb.PID, sb.FailedReason, sb.Pausing)
	}
}

// A delete that spends the pause budget after a good checkpoint leaves the pause context done, and the record must still say paused.
func TestPauseThatSpentItsBudgetStillRecordsTheCheckpoint(t *testing.T) {
	dir := t.TempDir()
	svc, l := newService(t, &recorder{}, running(), func(cfg *sandbox.Config) { cfg.PauseBudget = 50 * time.Millisecond })
	l.repo.checkpointDir = dir
	l.provider.spendBudget = true

	_, err := svc.Pause(t.Context(), "sandbox1")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "is paused") {
		t.Fatalf("pause returned %v, want the deadline and that the sandbox is paused", err)
	}

	if sb := l.repo.sb; sb.State != models.StatePaused || sb.PID != 0 || sb.Checkpoint != dir || sb.Pausing {
		t.Errorf("the record is %s with pid %d, checkpoint %q and mark %v, want paused with pid 0, %s and no mark", sb.State, sb.PID, sb.Checkpoint, sb.Pausing, dir)
	}
}

// A complete checkpoint outranks a failed host cleanup: the record must say paused, or start throws it away.
func TestPauseRecordsAPausedSandboxWhoseCleanupFailed(t *testing.T) {
	dir := t.TempDir()
	svc, l := newService(t, &recorder{}, running())
	l.repo.checkpointDir = dir
	l.provider.cleanupFails = true

	_, err := svc.Pause(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "is paused") {
		t.Fatalf("pause returned %v, want the failure and that the sandbox is paused", err)
	}

	if sb := l.repo.sb; sb.State != models.StatePaused || sb.Checkpoint != dir || sb.Pausing {
		t.Errorf("the record is %s with checkpoint %q and mark %v, want paused with %s and no mark", sb.State, sb.Checkpoint, sb.Pausing, dir)
	}
}

// A delete that fails after the swap leaves the sentry frozen beside a complete checkpoint, which the pause must release (SHARD-366).
func TestPauseReleasesASandboxItsCleanupLeftFrozen(t *testing.T) {
	dir := t.TempDir()
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.repo.checkpointDir = dir
	l.provider.cleanupFreezes = true

	_, err := svc.Pause(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "is paused") {
		t.Fatalf("pause returned %v, want the failure and that the sandbox is paused", err)
	}

	if sb := l.repo.sb; sb.State != models.StatePaused || sb.PID != 0 || sb.Checkpoint != dir || sb.Pausing {
		t.Errorf("the record is %s with pid %d, checkpoint %q and mark %v, want paused with pid 0, %s and no mark", sb.State, sb.PID, sb.Checkpoint, sb.Pausing, dir)
	}
	if !slices.Contains(r.snapshot(), "provider.Release") {
		t.Errorf("the calls were %v, want the frozen sandbox released: a resume refuses a live one", r.snapshot())
	}
}

// A release that fails keeps the mark over the checkpoint, so the liveness tick finishes the pause later.
func TestPauseKeepsItsMarkWhenTheReleaseFails(t *testing.T) {
	dir := t.TempDir()
	svc, l := newService(t, &recorder{fail: []string{"provider.Release"}}, running())
	l.repo.checkpointDir = dir
	l.provider.cleanupFreezes = true

	_, err := svc.Pause(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "release sandbox sandbox1") {
		t.Fatalf("pause returned %v, want the failed release", err)
	}

	if sb := l.repo.sb; sb.State != models.StateRunning || sb.Checkpoint != "" || !sb.Pausing {
		t.Errorf("the record is %s with checkpoint %q and mark %v, want running with no checkpoint and the mark", sb.State, sb.Checkpoint, sb.Pausing)
	}
}

// unreleasing is a substrate with no release, the way Firecracker is: its embedded interface hides the fake's Release.
type unreleasing struct{ models.Provider }

// A substrate that cannot release keeps its guest frozen beside the checkpoint, so the failed pause must keep the mark that names it.
func TestPauseKeepsItsMarkOverAFrozenSandboxTheSubstrateCannotRelease(t *testing.T) {
	dir := t.TempDir()
	r := &recorder{}
	svc, l := newService(t, r, running(), func(c *sandbox.Config) { c.Provider = unreleasing{c.Provider} })
	l.repo.checkpointDir = dir
	l.provider.cleanupFreezes = true

	_, err := svc.Pause(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "keeps the pause mark") {
		t.Fatalf("pause returned %v, want the failure and that the record keeps its mark", err)
	}

	if sb := l.repo.sb; sb.State != models.StateRunning || sb.Checkpoint != "" || !sb.Pausing {
		t.Errorf("the record is %s with checkpoint %q and mark %v, want running with no checkpoint and the mark", sb.State, sb.Checkpoint, sb.Pausing)
	}
	if slices.Contains(r.snapshot(), "provider.Release") {
		t.Errorf("the calls were %v, want no release from a substrate that has none", r.snapshot())
	}
}

// A stop ends the sandbox, so its checkpoint's memory and disk copy go with it rather than leak until rm (SHARD-592).
func TestStopDropsTheCheckpointOfAPausedSandbox(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), []byte("memory"), 0o600); err != nil {
		t.Fatal(err)
	}

	paused := pausedSandbox()
	paused.Checkpoint = dir
	svc, l := newService(t, &recorder{}, paused)
	l.repo.checkpointDir = dir

	sb, err := svc.Stop(t.Context(), "sandbox1")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}

	if sb.State != models.StateStopped || sb.Checkpoint != "" {
		t.Errorf("the record is %s with checkpoint %q, want stopped with none", sb.State, sb.Checkpoint)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the checkpoint directory survived the stop: %v", err)
	}
}

// A stop whose drop failed wrote a stopped record, so the next stop retries the drop rather than leak the checkpoint (SHARD-592).
func TestStopRetriesTheCheckpointDropAfterItsFirstFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes a directory's contents whatever its mode says, so no drop fails")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), []byte("memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Mode 0500 denies the removal of the directory's contents, so the first drop fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	paused := pausedSandbox()
	paused.Checkpoint = dir
	svc, l := newService(t, &recorder{}, paused)
	l.repo.checkpointDir = dir

	_, err := svc.Stop(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("first stop = %v, want the drop's permission error", err)
	}
	if l.repo.sb.State != models.StateStopped {
		t.Fatalf("the record is %s, want stopped after the failed drop", l.repo.sb.State)
	}
	if _, err := os.Stat(filepath.Join(dir, "checkpoint.img")); err != nil {
		t.Fatalf("the memory copy must remain after the refused drop: %v", err)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sb, err := svc.Stop(t.Context(), "sandbox1")
	if err != nil {
		t.Fatalf("retry stop: %v", err)
	}
	if sb.State != models.StateStopped || sb.Checkpoint != "" {
		t.Errorf("the record is %s with checkpoint %q, want stopped with none", sb.State, sb.Checkpoint)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the checkpoint directory survived the retry: %v", err)
	}
}

func TestResumeRunsAPausedSandboxAgain(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, pausedSandbox())
	l.provider.status = models.Status{}

	sb, err := svc.Resume(t.Context(), "web")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}

	if sb.ID != "sandbox1" || sb.State != models.StateRunning || sb.PID != 7 {
		t.Errorf("resume answered %+v, want sandbox1 running with pid 7", sb)
	}
	if l.provider.checkpointDir != "/checkpoints/sandbox1" {
		t.Errorf("the provider was told to read %q, want the checkpoint the record holds", l.provider.checkpointDir)
	}

	// gVisor took the address into the guest at create, so the netns is built again before the restore.
	if !l.net.allocated || slices.Index(r.calls, "net.Allocate") > slices.Index(r.calls, "provider.Resume") {
		t.Errorf("the network was not built again before the resume: %v", r.calls)
	}
	// The host rules are the policy of record, and the restored guest holds none of its own.
	if slices.Index(r.calls, "net.Reapply") < slices.Index(r.calls, "provider.Resume") {
		t.Errorf("the host rules were not applied again after the resume: %v", r.calls)
	}

	// The resumed run is the one the pause froze, so an exit its entrypoint already had still stands.
	if sb.ExitStatus == nil || sb.ExitStatus.Code != 3 {
		t.Errorf("the record lost its exit: %+v", sb.ExitStatus)
	}
	// A resume does not consume the checkpoint: the next one reads it again.
	if sb.Checkpoint != "/checkpoints/sandbox1" {
		t.Errorf("the record lost its checkpoint: %q", sb.Checkpoint)
	}
}

// A resume is no start, so ls counts UPTIME from the first one, while liveness still sees a new run (SHARD-770).
func TestResumeKeepsTheFirstStart(t *testing.T) {
	paused := pausedSandbox()
	paused.StartedAt = time.Now().Add(-time.Hour).UTC()
	paused.RunStartedAt = paused.StartedAt
	svc, l := newService(t, &recorder{}, paused)
	l.provider.status = models.Status{}

	sb, err := svc.Resume(t.Context(), "web")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}

	if !sb.StartedAt.Equal(paused.StartedAt) {
		t.Errorf("the resume moved the start to %s, want the first start %s", sb.StartedAt, paused.StartedAt)
	}
	if !sb.RunStartedAt.After(paused.RunStartedAt) {
		t.Errorf("the resumed run began at %s, want later than the frozen run's %s", sb.RunStartedAt, paused.RunStartedAt)
	}
}

func TestResumeRefusesASandboxThatIsNotPaused(t *testing.T) {
	for _, state := range []models.State{models.StateRunning, models.StateStopped, models.StateCreated} {
		sb := pausedSandbox()
		sb.State = state
		svc, l := newService(t, &recorder{}, sb)

		_, err := svc.Resume(t.Context(), "sandbox1")
		if err == nil || !strings.Contains(err.Error(), string(state)) {
			t.Errorf("resume of a %s sandbox returned %v, want a refusal that names the state", state, err)
		}
		if l.provider.resumed {
			t.Errorf("resume of a %s sandbox reached the provider", state)
		}
	}
}

func TestResumeRefusesARecordWithNoCheckpoint(t *testing.T) {
	sb := pausedSandbox()
	sb.Checkpoint = ""
	svc, l := newService(t, &recorder{}, sb)

	_, err := svc.Resume(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "no saved state to resume") {
		t.Errorf("resume returned %v, want a refusal that says there is no saved state", err)
	}
	if l.net.allocated {
		t.Error("resume built the network for a sandbox it could not resume")
	}
}

func TestResumeKeepsTheRecordPausedWhenTheProviderFails(t *testing.T) {
	svc, l := newService(t, &recorder{fail: []string{"provider.Resume"}}, pausedSandbox())
	l.provider.status = models.Status{}

	if _, err := svc.Resume(t.Context(), "sandbox1"); err == nil {
		t.Fatal("resume returned no error")
	}

	if sb := l.repo.sb; sb.State != models.StatePaused || sb.Checkpoint == "" {
		t.Errorf("the record is %s with checkpoint %q after a failed resume, want paused with its checkpoint", sb.State, sb.Checkpoint)
	}
}

// The sandbox is up when the rules fail, and only stop ends one, so the record must say running.
func TestResumeRecordsTheSandboxWhenTheRulesFail(t *testing.T) {
	svc, l := newService(t, &recorder{fail: []string{"net.Reapply"}}, pausedSandbox())
	l.provider.status = models.Status{}

	_, err := svc.Resume(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "rules were not applied again") {
		t.Fatalf("resume returned %v, want the failure and that the sandbox runs without its rules", err)
	}

	sb := l.repo.sb
	if sb.State != models.StateRunning || sb.PID != 7 {
		t.Errorf("the record is %s with pid %d, want running with pid 7", sb.State, sb.PID)
	}
	if sb.ExitStatus == nil || sb.Checkpoint == "" {
		t.Errorf("the record lost its exit or its checkpoint: %+v", sb)
	}
}

// A resume that failed after the substrate came up leaves a live sandbox, and only stop ends one.
func TestResumeRecordsASandboxThatCameUpUnderAFailedResume(t *testing.T) {
	svc, l := newService(t, &recorder{fail: []string{"provider.Resume"}}, pausedSandbox())
	l.provider.status = models.Status{Exists: true, State: models.StateRunning, PID: 9}

	_, err := svc.Resume(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "may be running") {
		t.Fatalf("resume returned %v, want the failure and the warning that the sandbox stays", err)
	}

	if sb := l.repo.sb; sb.State != models.StateRunning || sb.PID != 9 || sb.ExitStatus == nil {
		t.Errorf("the record is %s with pid %d and exit %v, want running with pid 9 and its exit kept", sb.State, sb.PID, sb.ExitStatus)
	}
}

func TestForkStartsANewSandboxFromACaptureOfTheRunningSource(t *testing.T) {
	r := &recorder{}
	source := forkSource()
	source.Image = "docker.io/library/alpine:3.20"
	source.Resources = models.Resources{MemoryMiB: 256}
	svc, l := newService(t, r, source)

	sb, err := svc.Fork(t.Context(), "web", sandbox.CopyRequest{Name: "web-2"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}

	if sb.ID != "sandbox2" || sb.Name != "web-2" {
		t.Errorf("fork answered %+v, want the new id under the new name", sb)
	}
	if l.provider.forkedFrom != "sandbox1" {
		t.Errorf("the provider was told to capture %q, want the running source", l.provider.forkedFrom)
	}
	if l.provider.spec.ID != "sandbox2" || l.provider.spec.Name != "web-2" || l.provider.spec.StateDir != "/state/sandbox2" {
		t.Errorf("the provider was handed %+v, want the fork's own id, name and state directory", l.provider.spec)
	}
	if l.provider.spec.Resources != source.Resources {
		t.Errorf("the fork was bound to %+v, want the source's %+v", l.provider.spec.Resources, source.Resources)
	}

	// The fork has its own netns, and the host rules go on again once the guest is up in it.
	want := []string{"net.Allocate", "provider.Fork", "net.Reapply"}
	if got := keep(r.calls, want...); !slices.Equal(got, want) {
		t.Errorf("the network was driven as %v, want %v", got, want)
	}

	if sb.Image != source.Image || sb.Resources != source.Resources {
		t.Errorf("the fork's record is %+v, want the source's image and bound", sb)
	}
	if sb.State != models.StateRunning || sb.PID != 7 {
		t.Errorf("the fork's record is %s with pid %d, want running with pid 7", sb.State, sb.PID)
	}
	if sb.NetnsPath != "/run/netns/sandbox2" || sb.HostInterface != "shardv2" {
		t.Errorf("the fork's record holds the network %+v, want its own netns and interface", sb)
	}
	// The capture carries the source's run, so the exit its entrypoint already had is the fork's too.
	if sb.ExitStatus == nil || sb.ExitStatus.Code != 3 {
		t.Errorf("the fork's record holds the exit %+v, want the source's", sb.ExitStatus)
	}

	// The source runs on: its record is as it was.
	if l.repo.sb.State != models.StateRunning || l.repo.sb.PID != 42 {
		t.Errorf("the source's record changed to %+v", l.repo.sb)
	}
	if l.repo.deleted {
		t.Error("a fork that succeeded deleted a record")
	}
}

// A copy the root has no room for is the request's fault, and its text names no bound a fork could set (SHARD-750, SHARD-751).
func TestForkTheRootHasNoRoomForIsABadRequest(t *testing.T) {
	svc, l := newService(t, &recorder{}, forkSource())
	l.provider.forkErr = fmt.Errorf("copy the checkpoint disk for sandbox sandbox2 on fake: %w", &bundle.NoRoomError{Bound: 4096 << 20, Copy: true})

	_, err := svc.Fork(t.Context(), "web", sandbox.CopyRequest{Name: "web-2"})

	if _, ok := errors.AsType[*sandbox.RequestError](err); !ok {
		t.Fatalf("fork = %v, want a request error", err)
	}
	if public, _ := sandbox.PublicText(err); strings.Contains(public, "sandbox2") || !strings.HasSuffix(public, "; remove a sandbox") {
		t.Errorf("public text = %q, want the refusal alone, ending on the fix", public)
	}
}

// A memory the root has no room for refuses the pause as the request's fault, not as a broken daemon (SHARD-750).
func TestPauseTheRootHasNoRoomForIsABadRequest(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())
	l.provider.pauseErr = fmt.Errorf("sandbox sandbox1 on fake: %w", &bundle.NoRoomError{Bound: 1024 << 20, Memory: true})

	_, err := svc.Pause(t.Context(), "sandbox1")

	if _, ok := errors.AsType[*sandbox.RequestError](err); !ok {
		t.Fatalf("pause = %v, want a request error", err)
	}
	if public, _ := sandbox.PublicText(err); !strings.HasPrefix(public, "the 1024 MiB of sandbox memory to save does not fit") {
		t.Errorf("public text = %q, want the memory refusal", public)
	}
	if sb := l.repo.sb; sb.State != models.StateRunning || sb.Pausing {
		t.Errorf("the record is %s with mark %v, want running and unmarked", sb.State, sb.Pausing)
	}
}

// A refused pause whose reconcile did not settle the sandbox broke the daemon, whatever the refusal was (SHARD-750).
func TestPauseTheRootHasNoRoomForStaysInternalWhenTheSandboxIsGone(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())
	l.provider.pauseErr = fmt.Errorf("sandbox sandbox1 on fake: %w", &bundle.NoRoomError{Bound: 1024 << 20, Memory: true})
	l.provider.status = models.Status{}

	_, err := svc.Pause(t.Context(), "sandbox1")

	if _, ok := errors.AsType[*sandbox.RequestError](err); ok {
		t.Fatalf("pause = %v, want no request error over a sandbox that is gone", err)
	}
	if err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Errorf("pause = %v, want the refusal and that the sandbox is gone", err)
	}
}

// A fork captures the source as it runs now, so a source that does not run is refused before anything is claimed (SHARD-457).
// A fork runs the source's guest, so it keeps the source's memory and never takes a VM's default.
func TestForkOnAVMKeepsTheSourceMemory(t *testing.T) {
	source := forkSource()
	source.Resources = models.Resources{MemoryMiB: 256}
	sizer := &memoryProvider{}
	svc, l := newService(t, &recorder{}, source, withMemory(sizer))

	sb, err := svc.Fork(t.Context(), "web", sandbox.CopyRequest{Name: "web-2"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if sb.Resources.MemoryMiB != 256 || l.provider.spec.Resources.MemoryMiB != 256 {
		t.Errorf("the fork's record holds memory %d and its guest ran with %d, want the source's 256", sb.Resources.MemoryMiB, l.provider.spec.Resources.MemoryMiB)
	}
}

func TestForkRefusesASourceThatDoesNotRun(t *testing.T) {
	for _, state := range []models.State{models.StatePaused, models.StateStopped, models.StateUnresponsive} {
		r := &recorder{}
		sb := forkSource()
		sb.State = state
		svc, _ := newService(t, r, sb)

		_, err := svc.Fork(t.Context(), "sandbox1", sandbox.CopyRequest{})
		var refused *sandbox.StateError
		if !errors.As(err, &refused) || refused.Code != models.CodeSandboxNotRunning || !strings.Contains(err.Error(), "fork takes a running sandbox") {
			t.Errorf("fork of a %s sandbox returned %v, want a not-running refusal", state, err)
		}
		if slices.Contains(r.calls, "repo.Create") || slices.Contains(r.calls, "provider.Fork") {
			t.Errorf("the refusal of a %s source came after a record or a capture: %v", state, r.calls)
		}
	}
}

// A bad name is the caller's spelling, so the API answers it 400, never 500. (SHARD-736)
func TestForkRefusesABadName(t *testing.T) {
	svc, _ := newService(t, &recorder{}, forkSource())

	for _, name := range []string{"Web 2", "brave-otter-1a2b"} {
		var invalid *sandboxstate.ValidationError
		if _, err := svc.Fork(t.Context(), "sandbox1", sandbox.CopyRequest{Name: name}); !errors.As(err, &invalid) {
			t.Errorf("fork named %q got %T %v, want a ValidationError", name, err, err)
		}
	}
}

// Everything claimed before the restore goes back when it fails, and the source is left alone.
func TestForkGivesBackWhatItClaimedWhenTheRestoreFails(t *testing.T) {
	r := &recorder{fail: []string{"provider.Fork"}}
	svc, l := newService(t, r, forkSource())

	if _, err := svc.Fork(t.Context(), "sandbox1", sandbox.CopyRequest{}); err == nil {
		t.Fatal("fork reported success when the restore failed")
	}

	want := []string{"provider.Fork", "provider.Remove", "net.Release", "repo.Delete"}
	if got := keep(r.calls, want...); !slices.Equal(got, want) {
		t.Errorf("the teardown ran as %v, want %v", got, want)
	}
	if l.repo.sb.State != models.StateRunning {
		t.Errorf("the source's record changed to %s", l.repo.sb.State)
	}
}

// A cancel during the source's capture lands before any restore, so nothing runs under the fork's claims and they all go back (SHARD-457).
func TestAForkCutBeforeItsRestoreGivesBackWhatItClaimed(t *testing.T) {
	r := &recorder{fail: []string{"provider.Fork"}}
	svc, l := newService(t, r, forkSource())
	ctx, cancel := context.WithCancel(t.Context())
	l.provider.onFork = func() {
		cancel()
		l.provider.status = models.Status{}
	}

	_, err := svc.Fork(ctx, "sandbox1", sandbox.CopyRequest{})
	if err == nil || strings.Contains(err.Error(), "stays on the host") {
		t.Fatalf("fork = %v, want the cut reported and nothing kept", err)
	}

	want := []string{"provider.Fork", "provider.Status", "provider.Remove", "net.Release", "repo.Delete"}
	if got := keep(r.calls, want...); !slices.Equal(got[len(got)-len(want):], want) {
		t.Errorf("the cut fork ran %v, want %v at the end", got, want)
	}
	for _, step := range []string{"provider.Remove", "net.Release"} {
		if !r.live[step] {
			t.Errorf("%s ran on the cancelled context, so it could not give anything back", step)
		}
	}
}

// A cancel can cut the restore after it started the guest, so a fork the substrate reports alive is kept (SHARD-457).
func TestAForkCutInItsRestoreKeepsAForkThatRuns(t *testing.T) {
	r := &recorder{fail: []string{"provider.Fork"}}
	svc, l := newService(t, r, forkSource())
	ctx, cancel := context.WithCancel(t.Context())
	l.provider.onFork = cancel

	_, err := svc.Fork(ctx, "sandbox1", sandbox.CopyRequest{})
	if err == nil || !strings.Contains(err.Error(), "sandbox2") || !strings.Contains(err.Error(), "stays on the host") {
		t.Fatalf("fork = %v, want the kept fork named", err)
	}

	for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("ran %s after a cut restore; the fork may already run", step)
		}
	}
}

// The copy is half-built while it is given back, so the teardown runs under its lock and no verb sees it.
func TestAFailedForkUnwindsBeforeItLetsTheCopyGo(t *testing.T) {
	r := &recorder{fail: []string{"provider.Fork"}}
	svc, l := newService(t, r, forkSource())

	reached := make(chan struct{})
	l.provider.onRemove = func() {
		go func() {
			_, _ = svc.Pause(t.Context(), "sandbox2")
			close(reached)
		}()

		select {
		case <-reached:
			t.Error("a verb reached the copy while the fork was still giving it back")
		case <-time.After(50 * time.Millisecond):
		}
	}

	if _, err := svc.Fork(t.Context(), "sandbox1", sandbox.CopyRequest{}); err == nil {
		t.Fatal("fork reported success when the restore failed")
	}

	// The same lock must be free once the fork is done, or every later verb on the copy hangs.
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("the copy's lock was never released")
	}
}

// An rm can take the copy's lock before the fork does, so the fork finds no record and claims nothing more (SHARD-582).
func TestAForkWhoseCopyWasRemovedFirstClaimsNothingMore(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, forkSource())
	l.repo.onCreate = func(id string) {
		l.provider.status = models.Status{}
		if err := svc.Remove(t.Context(), id, false); err != nil {
			t.Fatalf("rm of the copy: %v", err)
		}
	}

	_, err := svc.Fork(t.Context(), "sandbox1", sandbox.CopyRequest{})
	if err == nil || !errors.Is(err, sandboxstate.ErrNotFound) || strings.Contains(err.Error(), "left on the host") {
		t.Fatalf("fork = %v, want the copy reported removed and nothing reported left", err)
	}

	if got := keep(r.calls, "net.Allocate", "provider.Fork"); len(got) != 0 {
		t.Errorf("the fork went on to %v for a copy an rm had freed", got)
	}
}

// The fork is live once the restore returns, so a failure after it keeps the sandbox and its record.
func TestForkKeepsTheSandboxWhenTheRulesFail(t *testing.T) {
	r := &recorder{fail: []string{"net.Reapply"}}
	svc, l := newService(t, r, forkSource())

	if _, err := svc.Fork(t.Context(), "sandbox1", sandbox.CopyRequest{}); err == nil {
		t.Fatal("fork reported success when the rules failed")
	}

	if slices.Contains(r.calls, "provider.Remove") || slices.Contains(r.calls, "repo.Delete") {
		t.Errorf("a live fork was torn down: %v", r.calls)
	}
	// The record must name the netns and the interface, or a later rm cannot give them back.
	made := l.repo.made
	if made == nil || made.NetnsPath != "/run/netns/sandbox2" || made.Address.String() != "10.0.0.2/24" {
		t.Errorf("the fork's record holds the network %+v, want the one it was allocated", made)
	}
	if made.State != models.StateRunning || made.PID != 7 {
		t.Errorf("the fork's record is %s with pid %d, want running with its pid", made.State, made.PID)
	}
}

func TestForkCarriesThePolicyAndTellsTheHostBeforeTheRestore(t *testing.T) {
	r := &recorder{}
	source := forkSource()
	source.Policy = "locked"
	svc, l := newService(t, r, source)

	sb, err := svc.Fork(t.Context(), "sandbox1", sandbox.CopyRequest{})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}

	want := []string{"net.Allocate", "net.Reapply", "provider.Fork", "net.Reapply"}
	if got := keep(r.calls, "net.Allocate", "net.Reapply", "provider.Fork"); !slices.Equal(got, want) {
		t.Errorf("the network was driven as %v, want %v", got, want)
	}
	if sb.Policy != "locked" || l.repo.created.Policy != "locked" {
		t.Errorf("the fork names policy %q, want the source's", sb.Policy)
	}
	if got := l.provider.spec.Network.Nameservers; !slices.Equal(got, []netip.Addr{netip.MustParseAddr("10.0.0.1")}) {
		t.Errorf("the fork resolves through %v, want the gateway", got)
	}
}

// copyRunState is every record field a copy does not take from its source: its own identity, its run, and what the substrate reports.
var copyRunState = []string{"ID", "Name", "Provider", "State", "ExitStatus", "StoppedReason", "FailedReason", "FailedPublic", "UnresponsiveReason", "Checkpoint", "Pausing", "Snapshot",
	"PID", "NetnsPath", "Address", "HostInterface", "ExitChannel",
	"Restart", "StartedAt", "RunStartedAt", "CreatedAt", "ForkedFrom"}

// withEveryPolicy sets every field a create asks for, so a field a copy drops shows up as a difference.
func withEveryPolicy(sb models.Sandbox) models.Sandbox {
	sb.Image = "docker.io/library/alpine:3.20"
	sb.Digest = fakeDigest
	sb.Resources = models.Resources{MemoryMiB: 256, VCPUs: 2, DiskMiB: 1024}
	sb.Secrets = []string{"api-token"}
	sb.Policy = "locked"
	sb.Command = []string{"python", "-m", "http.server"}
	sb.Restart = &models.Restart{RestartSpec: models.RestartSpec{Policy: models.RestartOnFailure, Retries: 5, Backoff: 1}}
	// No create asks for it, but a fork runs the source's memory image and so its kernel (SHARD-745).
	sb.Kernel = "kernel-6.12.110-3"

	return sb
}

// Inspect reads the source of a fork from the record, and the fork's own first start is its own (SHARD-768).
func TestForkNamesItsSourceAndStartsItsOwnRun(t *testing.T) {
	source := forkSource()
	source.StartedAt = time.Now().Add(-time.Hour).UTC()
	svc, _ := newService(t, &recorder{}, source)

	sb, err := svc.Fork(t.Context(), "web", sandbox.CopyRequest{Name: "web-2"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}

	if sb.ForkedFrom != "sandbox1" {
		t.Errorf("the fork names its source %q, want sandbox1", sb.ForkedFrom)
	}
	if !sb.StartedAt.After(source.StartedAt) || !sb.StartedAt.Equal(sb.RunStartedAt) {
		t.Errorf("the fork started at %s and its run at %s, want both now, after the source's %s", sb.StartedAt, sb.RunStartedAt, source.StartedAt)
	}
}

func TestForkCarriesEveryPolicyField(t *testing.T) {
	source := withEveryPolicy(forkSource())
	svc, _ := newService(t, &recorder{}, source)
	sb, err := svc.Fork(t.Context(), "web", sandbox.CopyRequest{Name: "web-2"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}

	src, copied := reflect.ValueOf(source), reflect.ValueOf(sb)
	for _, field := range reflect.VisibleFields(src.Type()) {
		if slices.Contains(copyRunState, field.Name) {
			continue
		}
		// A field the source leaves zero would pass whether or not the copy carries it.
		if src.FieldByIndex(field.Index).IsZero() {
			t.Errorf("the source leaves %s zero, so the test proves nothing about it: set it in withEveryPolicy", field.Name)
			continue
		}
		if want, got := src.FieldByIndex(field.Index).Interface(), copied.FieldByIndex(field.Index).Interface(); !reflect.DeepEqual(got, want) {
			t.Errorf("the fork holds %s %v, want the source's %v", field.Name, got, want)
		}
	}
	if sb.Restart == nil || sb.Restart.RestartSpec != source.Restart.RestartSpec {
		t.Errorf("the fork holds the restart %+v, want the source's policy %+v", sb.Restart, source.Restart.RestartSpec)
	}
}

// A resume over an image an rm deleted names the pull that brings it back, and the record stays paused (SHARD-585).
func TestResumeOverAGoneImageNamesThePullThatBringsItBack(t *testing.T) {
	svc, l := newService(t, &recorder{fail: []string{"provider.Resume"}, cause: goneImage()}, withImage(pausedSandbox()))
	l.provider.status = models.Status{}

	_, err := svc.Resume(t.Context(), "sandbox1")

	imageGone(t, err, "resume")
	if l.repo.sb.State != models.StatePaused {
		t.Errorf("the record is %s after the refused resume, want paused", l.repo.sb.State)
	}
}

// A fork mounts the source's image afresh, so one an rm deleted names the pull that brings it back (SHARD-585).
func TestForkOverAGoneImageNamesThePullThatBringsItBack(t *testing.T) {
	svc, _ := newService(t, &recorder{fail: []string{"provider.Fork"}, cause: goneImage()}, withImage(forkSource()))

	_, err := svc.Fork(t.Context(), "sandbox1", sandbox.CopyRequest{})

	imageGone(t, err, "fork")
}
