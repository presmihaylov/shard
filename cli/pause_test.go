package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

func TestPauseWritesTheSnapshotAndRecordsIt(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	sb := running()
	sb.Name = "web"
	sb.ExitStatus = &models.ExitStatus{Code: 3}
	app, d := newLifecycleApp(t, &out, r, sb)

	if err := app.Run(t.Context(), []string{"pause", "web"}); err != nil {
		t.Fatalf("pause: %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != "sandbox1" {
		t.Errorf("pause printed %q, want the id", got)
	}

	if got := d.providerSvc.(*fakeLifecycleProvider).snapshot; got != "/snapshots/sandbox1" {
		t.Errorf("the provider was told to write %q, want the repository's snapshot directory", got)
	}

	got := d.repoSvc.(*fakeLifecycleRepo).sb
	if got.State != models.StatePaused || got.PID != 0 || got.Snapshot != "/snapshots/sandbox1" {
		t.Errorf("the record is %s with pid %d and snapshot %q, want paused, 0 and the snapshot", got.State, got.PID, got.Snapshot)
	}
	// The entrypoint's exit is part of the run the snapshot froze.
	if got.ExitStatus == nil || got.ExitStatus.Code != 3 {
		t.Errorf("the record lost its exit: %+v", got.ExitStatus)
	}

	if slices.Index(r.calls, "provider.Pause") > slices.Index(r.calls, "repo.Update") {
		t.Errorf("the record was updated before the pause: %v", r.calls)
	}
}

func TestPauseRefusesASandboxThatIsNotRunning(t *testing.T) {
	for _, state := range []models.State{models.StateStopped, models.StateCreated, models.StatePaused} {
		sb := running()
		sb.State = state
		app, d := newLifecycleApp(t, &bytes.Buffer{}, &recorder{}, sb)

		err := app.Run(t.Context(), []string{"pause", "sandbox1"})
		if err == nil || !strings.Contains(err.Error(), string(state)) {
			t.Errorf("pause of a %s sandbox returned %v, want a refusal that names the state", state, err)
		}
		if d.providerSvc.(*fakeLifecycleProvider).snapshot != "" {
			t.Errorf("pause of a %s sandbox reached the provider", state)
		}
	}
}

func TestPauseKeepsTheRecordRunningWhenTheSandboxStillIs(t *testing.T) {
	app, d := newLifecycleApp(t, &bytes.Buffer{}, &recorder{fail: []string{"provider.Pause"}}, running())

	if err := app.Run(t.Context(), []string{"pause", "sandbox1"}); err == nil {
		t.Fatal("pause returned no error")
	}

	if sb := d.repoSvc.(*fakeLifecycleRepo).sb; sb.State != models.StateRunning || sb.Snapshot != "" {
		t.Errorf("the record is %s with snapshot %q after a failed pause, want running with none", sb.State, sb.Snapshot)
	}
}

// A pause that lost the sandbox on the way must say so, or rm refuses a record that says running.
func TestPauseRecordsASandboxTheProviderLost(t *testing.T) {
	r := &recorder{fail: []string{"provider.Pause"}}
	app, d := newLifecycleApp(t, &bytes.Buffer{}, r, running())
	d.providerSvc.(*fakeLifecycleProvider).status = models.Status{}

	err := app.Run(t.Context(), []string{"pause", "sandbox1"})
	if err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("pause returned %v, want the failure and that the sandbox is gone", err)
	}

	if sb := d.repoSvc.(*fakeLifecycleRepo).sb; sb.State != models.StateStopped || sb.PID != 0 {
		t.Errorf("the record is %s with pid %d, want stopped with pid 0", sb.State, sb.PID)
	}
}

// A complete snapshot outranks a failed host cleanup: the record must say paused, or start throws it away.
func TestPauseRecordsAPausedSandboxWhoseCleanupFailed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, checkpointFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	r := &recorder{fail: []string{"provider.Pause"}}
	app, d := newLifecycleApp(t, &bytes.Buffer{}, r, running())
	d.repoSvc.(*fakeLifecycleRepo).snapshotDir = dir
	d.providerSvc.(*fakeLifecycleProvider).status = models.Status{}

	err := app.Run(t.Context(), []string{"pause", "sandbox1"})
	if err == nil || !strings.Contains(err.Error(), "is paused") {
		t.Fatalf("pause returned %v, want the failure and that the sandbox is paused", err)
	}

	if sb := d.repoSvc.(*fakeLifecycleRepo).sb; sb.State != models.StatePaused || sb.Snapshot != dir {
		t.Errorf("the record is %s with snapshot %q, want paused with %s", sb.State, sb.Snapshot, dir)
	}
}

func TestPauseHoldsTheSandboxBeforeItReadsTheRecord(t *testing.T) {
	r := &recorder{live: map[string]bool{}}
	app, _ := newLifecycleApp(t, &bytes.Buffer{}, r, running())

	if err := app.Run(t.Context(), []string{"pause", "sandbox1"}); err != nil {
		t.Fatalf("pause: %v", err)
	}

	if slices.Index(r.calls, "repo.Hold") > slices.Index(r.calls, "repo.Get") {
		t.Errorf("the record was read before the sandbox was held: %v", r.calls)
	}
}
