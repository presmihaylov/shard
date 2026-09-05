package sandbox_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

func TestPauseWritesTheSnapshotAndRecordsIt(t *testing.T) {
	r := &recorder{}
	source := running()
	source.Name = "web"
	source.ExitStatus = &models.ExitStatus{Code: 3}
	svc, l := newService(t, r, source)

	sb, err := svc.Pause(t.Context(), "web")
	if err != nil {
		t.Fatalf("pause: %v", err)
	}

	if sb.ID != "sandbox1" || sb.State != models.StatePaused || sb.PID != 0 || sb.Snapshot != "/snapshots/sandbox1" {
		t.Errorf("pause answered %+v, want sandbox1 paused with pid 0 and its snapshot", sb)
	}
	if l.provider.snapshotDir != "/snapshots/sandbox1" {
		t.Errorf("the provider was told to write %q, want the repository's snapshot directory", l.provider.snapshotDir)
	}
	// The entrypoint's exit is part of the run the snapshot froze.
	if sb.ExitStatus == nil || sb.ExitStatus.Code != 3 {
		t.Errorf("the record lost its exit: %+v", sb.ExitStatus)
	}

	if slices.Index(r.calls, "provider.Pause") > slices.Index(r.calls, "repo.Update") {
		t.Errorf("the record was updated before the pause: %v", r.calls)
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

func TestPauseKeepsTheRecordRunningWhenTheSandboxStillIs(t *testing.T) {
	svc, l := newService(t, &recorder{fail: []string{"provider.Pause"}}, running())

	if _, err := svc.Pause(t.Context(), "sandbox1"); err == nil {
		t.Fatal("pause returned no error")
	}

	if sb := l.repo.sb; sb.State != models.StateRunning || sb.Snapshot != "" {
		t.Errorf("the record is %s with snapshot %q after a failed pause, want running with none", sb.State, sb.Snapshot)
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

// A complete snapshot outranks a failed host cleanup: the record must say paused, or start throws it away.
func TestPauseRecordsAPausedSandboxWhoseCleanupFailed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	svc, l := newService(t, &recorder{fail: []string{"provider.Pause"}}, running())
	l.repo.snapshotDir = dir
	l.provider.status = models.Status{}

	_, err := svc.Pause(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "is paused") {
		t.Fatalf("pause returned %v, want the failure and that the sandbox is paused", err)
	}

	if sb := l.repo.sb; sb.State != models.StatePaused || sb.Snapshot != dir {
		t.Errorf("the record is %s with snapshot %q, want paused with %s", sb.State, sb.Snapshot, dir)
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
	if l.provider.snapshotDir != "/snapshots/sandbox1" {
		t.Errorf("the provider was told to read %q, want the snapshot the record holds", l.provider.snapshotDir)
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
	// A resume does not consume the snapshot: the next one reads it again.
	if sb.Snapshot != "/snapshots/sandbox1" {
		t.Errorf("the record lost its snapshot: %q", sb.Snapshot)
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

func TestResumeRefusesARecordWithNoSnapshot(t *testing.T) {
	sb := pausedSandbox()
	sb.Snapshot = ""
	svc, l := newService(t, &recorder{}, sb)

	_, err := svc.Resume(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "no snapshot") {
		t.Errorf("resume returned %v, want a refusal that says there is no snapshot", err)
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

	if sb := l.repo.sb; sb.State != models.StatePaused || sb.Snapshot == "" {
		t.Errorf("the record is %s with snapshot %q after a failed resume, want paused with its snapshot", sb.State, sb.Snapshot)
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
	if sb.ExitStatus == nil || sb.Snapshot == "" {
		t.Errorf("the record lost its exit or its snapshot: %+v", sb)
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

func TestForkStartsANewSandboxFromTheSnapshot(t *testing.T) {
	r := &recorder{}
	source := pausedSandbox()
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
	if l.provider.snapshotDir != "/snapshots/sandbox1" {
		t.Errorf("the provider was told to read %q, want the source's snapshot", l.provider.snapshotDir)
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
	// The memory image carries the source's run, so the exit its entrypoint already had is the fork's too.
	if sb.ExitStatus == nil || sb.ExitStatus.Code != 3 {
		t.Errorf("the fork's record holds the exit %+v, want the source's", sb.ExitStatus)
	}

	// The source is read and nothing more: its record and its snapshot are as they were.
	if l.repo.sb.State != models.StatePaused || l.repo.sb.Snapshot != "/snapshots/sandbox1" {
		t.Errorf("the source's record changed to %+v", l.repo.sb)
	}
	if l.repo.deleted {
		t.Error("a fork that succeeded deleted a record")
	}
}

// A fork reads the snapshot, so the source may be in any state that has one, the running one included.
func TestForkTakesASourceInAnyStateThatHasASnapshot(t *testing.T) {
	for _, state := range []models.State{models.StateRunning, models.StateStopped, models.StatePaused} {
		sb := pausedSandbox()
		sb.State = state
		svc, _ := newService(t, &recorder{}, sb)

		if _, err := svc.Fork(t.Context(), "sandbox1", sandbox.CopyRequest{}); err != nil {
			t.Errorf("fork of a %s sandbox: %v", state, err)
		}
	}
}

func TestForkRefusesASourceWithNoSnapshot(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, running())

	_, err := svc.Fork(t.Context(), "sandbox1", sandbox.CopyRequest{})
	if err == nil || !strings.Contains(err.Error(), "pause it first") {
		t.Errorf("fork of a sandbox that was never paused returned %v, want a refusal that says so", err)
	}
	if slices.Contains(r.calls, "repo.Create") {
		t.Error("the refusal came after a record was created")
	}
}

func TestForkRefusesABadName(t *testing.T) {
	svc, _ := newService(t, &recorder{}, pausedSandbox())

	if _, err := svc.Fork(t.Context(), "sandbox1", sandbox.CopyRequest{Name: "Web 2"}); err == nil {
		t.Error("fork accepted a name no verb could take back")
	}
}

// Everything claimed before the restore goes back when it fails, and the source is left alone.
func TestForkGivesBackWhatItClaimedWhenTheRestoreFails(t *testing.T) {
	r := &recorder{fail: []string{"provider.Fork"}}
	svc, l := newService(t, r, pausedSandbox())

	if _, err := svc.Fork(t.Context(), "sandbox1", sandbox.CopyRequest{}); err == nil {
		t.Fatal("fork reported success when the restore failed")
	}

	want := []string{"provider.Fork", "provider.Remove", "net.Release", "repo.Delete"}
	if got := keep(r.calls, want...); !slices.Equal(got, want) {
		t.Errorf("the teardown ran as %v, want %v", got, want)
	}
	if l.repo.sb.State != models.StatePaused {
		t.Errorf("the source's record changed to %s", l.repo.sb.State)
	}
}

// The fork is live once the restore returns, so a failure after it keeps the sandbox and its record.
func TestForkKeepsTheSandboxWhenTheRulesFail(t *testing.T) {
	r := &recorder{fail: []string{"net.Reapply"}}
	svc, l := newService(t, r, pausedSandbox())

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
	source := pausedSandbox()
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
}

// cloneSource is a stopped sandbox with the image and the bound a clone must carry over.
func cloneSource() models.Sandbox {
	sb := stopped()
	sb.Image = "docker.io/library/alpine:3.20"
	sb.Resources = models.Resources{MemoryMiB: 256}

	return sb
}

func TestCloneStartsANewSandboxOverTheSourcesFiles(t *testing.T) {
	r := &recorder{}
	source := cloneSource()
	svc, l := newService(t, r, source)

	sb, err := svc.Clone(t.Context(), "web", sandbox.CopyRequest{Name: "web-2"})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	if sb.ID != "sandbox2" || sb.Name != "web-2" {
		t.Errorf("clone answered %+v, want the new id under the new name", sb)
	}
	if l.provider.source != "sandbox1" {
		t.Errorf("the provider was told to copy %q, want the source's id", l.provider.source)
	}
	if l.provider.spec.ID != "sandbox2" || l.provider.spec.StateDir != "/state/sandbox2" {
		t.Errorf("the provider was handed %+v, want the clone's own id and state directory", l.provider.spec)
	}
	if l.provider.spec.Network.NetnsPath != "/run/netns/sandbox2" {
		t.Errorf("the provider was handed the network %+v, want the clone's own netns", l.provider.spec.Network)
	}

	// The clone comes up in a fresh netns the allocation ruled, as a create does, so nothing is applied again.
	want := []string{"net.Allocate", "provider.Clone"}
	if got := keep(r.calls, "net.Allocate", "provider.Clone", "net.Reapply"); !slices.Equal(got, want) {
		t.Errorf("the network was driven as %v, want %v", got, want)
	}

	if sb.Image != source.Image || sb.Resources != source.Resources {
		t.Errorf("the clone's record is %+v, want the source's image and bound", sb)
	}
	if sb.State != models.StateRunning || sb.PID != 7 {
		t.Errorf("the clone's record is %s with pid %d, want running with pid 7", sb.State, sb.PID)
	}
	// The entrypoint runs from the beginning, so the source's exit says nothing about the clone.
	if sb.ExitStatus != nil {
		t.Errorf("the clone's record holds the exit %+v, want none", sb.ExitStatus)
	}
	if sb.Snapshot != "" {
		t.Errorf("the clone's record names the snapshot %q, want none", sb.Snapshot)
	}

	if l.repo.sb.State != models.StateStopped || l.repo.sb.ExitStatus == nil {
		t.Errorf("the source's record changed to %+v", l.repo.sb)
	}
	if l.repo.deleted {
		t.Error("a clone that succeeded deleted a record")
	}
}

// A paused sandbox holds its files as they were at the pause, and nothing writes them, so it clones too.
func TestCloneTakesAPausedSource(t *testing.T) {
	svc, l := newService(t, &recorder{}, pausedSandbox())

	if _, err := svc.Clone(t.Context(), "sandbox1", sandbox.CopyRequest{}); err != nil {
		t.Fatalf("clone of a paused sandbox: %v", err)
	}
	if l.repo.sb.State != models.StatePaused || l.repo.sb.Snapshot == "" {
		t.Errorf("the source's record changed to %+v", l.repo.sb)
	}
}

func TestCloneRefusesASourceThatIsUp(t *testing.T) {
	for _, state := range []models.State{models.StateRunning, models.StateCreated} {
		sb := cloneSource()
		sb.State = state
		r := &recorder{}
		svc, _ := newService(t, r, sb)

		_, err := svc.Clone(t.Context(), "sandbox1", sandbox.CopyRequest{})
		if err == nil || !strings.Contains(err.Error(), "stop it first") {
			t.Errorf("clone of a %s sandbox returned %v, want a refusal that names the stop", state, err)
		}
		if slices.Contains(r.calls, "repo.Create") {
			t.Error("the refusal came after a record was created")
		}
	}
}

// Everything claimed before the start goes back when it fails, and the source is left alone.
func TestCloneGivesBackWhatItClaimedWhenTheStartFails(t *testing.T) {
	r := &recorder{fail: []string{"provider.Clone"}}
	svc, l := newService(t, r, cloneSource())

	if _, err := svc.Clone(t.Context(), "sandbox1", sandbox.CopyRequest{}); err == nil {
		t.Fatal("clone reported success when the start failed")
	}

	want := []string{"provider.Clone", "provider.Remove", "net.Release", "repo.Delete"}
	if got := keep(r.calls, want...); !slices.Equal(got, want) {
		t.Errorf("the teardown ran as %v, want %v", got, want)
	}
	if l.repo.sb.State != models.StateStopped {
		t.Errorf("the source's record changed to %s", l.repo.sb.State)
	}
}

// The clone is live once the start returns, so a failure after it keeps the sandbox and its record.
func TestCloneKeepsTheSandboxWhenTheRecordWriteFails(t *testing.T) {
	r := &recorder{fail: []string{"provider.Status"}}
	svc, l := newService(t, r, cloneSource())

	if _, err := svc.Clone(t.Context(), "sandbox1", sandbox.CopyRequest{}); err == nil {
		t.Fatal("clone reported success when the status read failed")
	}

	if slices.Contains(r.calls, "provider.Remove") || slices.Contains(r.calls, "repo.Delete") {
		t.Errorf("a live clone was torn down: %v", r.calls)
	}
	// The record must name the netns and the interface, or a later rm cannot give them back.
	made := l.repo.made
	if made == nil || made.NetnsPath != "/run/netns/sandbox2" || made.Address.String() != "10.0.0.2/24" {
		t.Errorf("the clone's record holds the network %+v, want the one it was allocated", made)
	}
}

func TestCloneCarriesThePolicyAndTellsTheHostBeforeTheStart(t *testing.T) {
	r := &recorder{}
	source := cloneSource()
	source.Policy = "locked"
	svc, _ := newService(t, r, source)

	sb, err := svc.Clone(t.Context(), "sandbox1", sandbox.CopyRequest{})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	want := []string{"net.Allocate", "net.Reapply", "provider.Clone"}
	if got := keep(r.calls, want...); !slices.Equal(got, want) {
		t.Errorf("the network was driven as %v, want %v", got, want)
	}
	if sb.Policy != "locked" {
		t.Errorf("the clone names policy %q, want the source's", sb.Policy)
	}
}
