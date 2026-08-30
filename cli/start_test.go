package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

func stopped() models.Sandbox {
	return models.Sandbox{ID: "sandbox1", Name: "web", State: models.StateStopped, ExitStatus: &models.ExitStatus{Code: 3}}
}

func TestStartRunsAStoppedSandboxAgain(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, d := newLifecycleApp(t, &out, r, stopped())

	if err := app.Run(t.Context(), []string{"start", "web"}); err != nil {
		t.Fatalf("start: %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != "sandbox1" {
		t.Errorf("start printed %q, want the id", got)
	}

	provider := d.providerSvc.(*fakeLifecycleProvider)
	if !provider.started {
		t.Error("the provider was never asked to start")
	}

	// gVisor took the guest address at the first create, so the netns is built again before the run.
	if !d.netSvc.(*fakeLifecycleNet).allocated || slices.Index(r.calls, "net.Allocate") > slices.Index(r.calls, "provider.Start") {
		t.Errorf("the network was not built again before the start: %v", r.calls)
	}

	sb := d.repoSvc.(*fakeLifecycleRepo).sb
	if sb.State != models.StateRunning || sb.PID != 7 {
		t.Errorf("the record is %s with pid %d, want running with pid 7", sb.State, sb.PID)
	}
	if sb.ExitStatus != nil {
		t.Errorf("the record still holds the old exit %+v", *sb.ExitStatus)
	}

	// The record says running only once the sandbox is, so a failed start leaves it stopped.
	if slices.Index(r.calls, "provider.Start") > slices.Index(r.calls, "repo.Update") {
		t.Errorf("the record was updated before the start: %v", r.calls)
	}
}

func TestStartRefusesASandboxThatIsNotStopped(t *testing.T) {
	for _, state := range []models.State{models.StateRunning, models.StateCreated} {
		var out bytes.Buffer

		sb := running()
		sb.State = state
		app, d := newLifecycleApp(t, &out, &recorder{}, sb)

		err := app.Run(t.Context(), []string{"start", "sandbox1"})
		if err == nil || !strings.Contains(err.Error(), string(state)) {
			t.Errorf("start of a %s sandbox returned %v, want a refusal that names the state", state, err)
		}
		if d.providerSvc.(*fakeLifecycleProvider).started {
			t.Errorf("start of a %s sandbox reached the provider", state)
		}
	}
}

func TestStartKeepsTheRecordStoppedWhenTheProviderFails(t *testing.T) {
	var out bytes.Buffer

	app, d := newLifecycleApp(t, &out, &recorder{fail: []string{"provider.Start"}}, stopped())

	if err := app.Run(t.Context(), []string{"start", "sandbox1"}); err == nil {
		t.Fatal("start returned no error")
	}

	if sb := d.repoSvc.(*fakeLifecycleRepo).sb; sb.State != models.StateStopped || sb.ExitStatus == nil {
		t.Errorf("the record is %s with exit %v after a failed start, want stopped with its exit kept", sb.State, sb.ExitStatus)
	}
}

// A start that failed after the substrate came up leaves a live sandbox, and only stop ends one.
func TestStartRecordsASandboxThatCameUpUnderAFailedStart(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{fail: []string{"provider.Start"}}
	app, d := newLifecycleApp(t, &out, r, stopped())
	d.providerSvc.(*fakeLifecycleProvider).status = models.Status{Exists: true, State: models.StateRunning, PID: 9}

	err := app.Run(t.Context(), []string{"start", "sandbox1"})
	if err == nil || !strings.Contains(err.Error(), "may be running") {
		t.Fatalf("start returned %v, want the failure and the warning that the sandbox stays", err)
	}

	if sb := d.repoSvc.(*fakeLifecycleRepo).sb; sb.State != models.StateRunning || sb.PID != 9 {
		t.Errorf("the record is %s with pid %d, want running with pid 9", sb.State, sb.PID)
	}
}

func TestStartNamesTheSandboxWhenTheRecordWriteFails(t *testing.T) {
	var out bytes.Buffer

	app, _ := newLifecycleApp(t, &out, &recorder{fail: []string{"repo.Update"}}, stopped())

	err := app.Run(t.Context(), []string{"start", "sandbox1"})
	if err == nil || !strings.Contains(err.Error(), "is running but its record was not updated") {
		t.Errorf("start returned %v, want the sandbox named as running", err)
	}
}

// Two starts of one sandbox must not both build its netns: the second holds the sandbox first and
// then reads a record the first one has already flipped to running.
func TestStartHoldsTheSandboxBeforeItReadsTheRecord(t *testing.T) {
	r := &recorder{live: map[string]bool{}}
	app, _ := newLifecycleApp(t, &bytes.Buffer{}, r, stopped())

	if err := app.Run(t.Context(), []string{"start", stopped().ID}); err != nil {
		t.Fatalf("start: %v", err)
	}

	if held, read := slices.Index(r.calls, "repo.Hold"), slices.Index(r.calls, "repo.Get"); held < 0 || held > read {
		t.Errorf("the record was read before the sandbox was held: %v", r.calls)
	}
}
