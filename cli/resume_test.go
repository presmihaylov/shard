package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

func paused() models.Sandbox {
	return models.Sandbox{ID: "sandbox1", Name: "web", State: models.StatePaused, Snapshot: "/snapshots/sandbox1",
		ExitStatus: &models.ExitStatus{Code: 3}}
}

func TestResumeRunsAPausedSandboxAgain(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, d := newLifecycleApp(t, &out, r, paused())
	d.providerSvc.(*fakeLifecycleProvider).status = models.Status{}

	if err := app.Run(t.Context(), []string{"resume", "web"}); err != nil {
		t.Fatalf("resume: %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != "sandbox1" {
		t.Errorf("resume printed %q, want the id", got)
	}

	if got := d.providerSvc.(*fakeLifecycleProvider).snapshot; got != "/snapshots/sandbox1" {
		t.Errorf("the provider was told to read %q, want the snapshot the record holds", got)
	}

	// gVisor took the address into the guest at create, so the netns is built again before the restore.
	if !d.netSvc.(*fakeLifecycleNet).allocated || slices.Index(r.calls, "net.Allocate") > slices.Index(r.calls, "provider.Resume") {
		t.Errorf("the network was not built again before the resume: %v", r.calls)
	}

	sb := d.repoSvc.(*fakeLifecycleRepo).sb
	if sb.State != models.StateRunning || sb.PID != 7 {
		t.Errorf("the record is %s with pid %d, want running with pid 7", sb.State, sb.PID)
	}
	// The resumed run is the one the pause froze, so an exit its entrypoint already had still stands.
	if sb.ExitStatus == nil || sb.ExitStatus.Code != 3 {
		t.Errorf("the record lost its exit: %+v", sb.ExitStatus)
	}
	// A resume does not consume the snapshot: the next one reads it again.
	if sb.Snapshot != "/snapshots/sandbox1" {
		t.Errorf("the record lost its snapshot: %q", sb.Snapshot)
	}

	if slices.Index(r.calls, "provider.Resume") > slices.Index(r.calls, "repo.Update") {
		t.Errorf("the record was updated before the resume: %v", r.calls)
	}
}

func TestResumeRefusesASandboxThatIsNotPaused(t *testing.T) {
	for _, state := range []models.State{models.StateRunning, models.StateStopped, models.StateCreated} {
		sb := paused()
		sb.State = state
		app, d := newLifecycleApp(t, &bytes.Buffer{}, &recorder{}, sb)

		err := app.Run(t.Context(), []string{"resume", "sandbox1"})
		if err == nil || !strings.Contains(err.Error(), string(state)) {
			t.Errorf("resume of a %s sandbox returned %v, want a refusal that names the state", state, err)
		}
		if d.providerSvc.(*fakeLifecycleProvider).snapshot != "" {
			t.Errorf("resume of a %s sandbox reached the provider", state)
		}
	}
}

func TestResumeRefusesARecordWithNoSnapshot(t *testing.T) {
	sb := paused()
	sb.Snapshot = ""
	app, d := newLifecycleApp(t, &bytes.Buffer{}, &recorder{}, sb)

	err := app.Run(t.Context(), []string{"resume", "sandbox1"})
	if err == nil || !strings.Contains(err.Error(), "no snapshot") {
		t.Errorf("resume returned %v, want a refusal that says there is no snapshot", err)
	}
	if d.netSvc.(*fakeLifecycleNet).allocated {
		t.Error("resume built the network for a sandbox it could not resume")
	}
}

func TestResumeKeepsTheRecordPausedWhenTheProviderFails(t *testing.T) {
	app, d := newLifecycleApp(t, &bytes.Buffer{}, &recorder{fail: []string{"provider.Resume"}}, paused())
	d.providerSvc.(*fakeLifecycleProvider).status = models.Status{}

	if err := app.Run(t.Context(), []string{"resume", "sandbox1"}); err == nil {
		t.Fatal("resume returned no error")
	}

	if sb := d.repoSvc.(*fakeLifecycleRepo).sb; sb.State != models.StatePaused || sb.Snapshot == "" {
		t.Errorf("the record is %s with snapshot %q after a failed resume, want paused with its snapshot", sb.State, sb.Snapshot)
	}
}

// A resume that failed after the substrate came up leaves a live sandbox, and only stop ends one.
func TestResumeRecordsASandboxThatCameUpUnderAFailedResume(t *testing.T) {
	r := &recorder{fail: []string{"provider.Resume"}}
	app, d := newLifecycleApp(t, &bytes.Buffer{}, r, paused())
	d.providerSvc.(*fakeLifecycleProvider).status = models.Status{Exists: true, State: models.StateRunning, PID: 9}

	err := app.Run(t.Context(), []string{"resume", "sandbox1"})
	if err == nil || !strings.Contains(err.Error(), "may be running") {
		t.Fatalf("resume returned %v, want the failure and the warning that the sandbox stays", err)
	}

	if sb := d.repoSvc.(*fakeLifecycleRepo).sb; sb.State != models.StateRunning || sb.PID != 9 || sb.ExitStatus == nil {
		t.Errorf("the record is %s with pid %d and exit %v, want running with pid 9 and its exit kept", sb.State, sb.PID, sb.ExitStatus)
	}
}
