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
