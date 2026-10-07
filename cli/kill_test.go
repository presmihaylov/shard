package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

func newKillApp(t *testing.T, out *bytes.Buffer, sb models.Sandbox) (App, *fakeDaemon, *recorder) {
	t.Helper()

	sb.Processes = []models.Process{{Name: "api", Command: []string{"python", "app.py"}, Restart: models.RestartSpec{Policy: models.RestartUnlessStopped, Backoff: 1}, Status: models.ProcessStatus{State: models.ProcessRunning}}}
	r := &recorder{}
	app, d := newLifecycleApp(t, out, r, sb)

	return app, d, r
}

// A kill gives the process the stop grace and marks it, so no restart and no later start brings it back.
func TestKillEndsTheProcessAndKeepsItDown(t *testing.T) {
	var out bytes.Buffer

	app, d, _ := newKillApp(t, &out, running())

	if err := app.Run(t.Context(), []string{"kill", "sandbox1", "api"}); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if got := out.String(); got != "api\n" {
		t.Errorf("kill printed %q, want the process name", got)
	}
	if got := d.providerSvc.(*fakeLifecycleProvider).killed(); !slices.Equal(got, []time.Duration{models.StopGrace}) {
		t.Errorf("kill gave graces %v, want the stop grace", got)
	}

	p := d.repoSvc.(*fakeLifecycleRepo).sb.Processes[0]
	if !p.Killed || p.Status.State != models.ProcessKilled {
		t.Errorf("the record holds %+v, want api killed and marked", p)
	}
}

func TestKillForceTakesNoGraceOnEitherSide(t *testing.T) {
	for _, args := range [][]string{{"kill", "--force", "sandbox1", "api"}, {"kill", "sandbox1", "api", "--force"}} {
		var out bytes.Buffer

		app, d, _ := newKillApp(t, &out, running())

		if err := app.Run(t.Context(), args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if got := d.providerSvc.(*fakeLifecycleProvider).killed(); !slices.Equal(got, []time.Duration{0}) {
			t.Errorf("%v gave graces %v, want none", args, got)
		}
	}
}

// Nothing runs in a stopped sandbox, so the kill is the mark alone and the guest is never asked.
func TestKillOfAStoppedSandboxOnlyMarksTheProcess(t *testing.T) {
	var out bytes.Buffer

	app, d, r := newKillApp(t, &out, stopped())

	if err := app.Run(t.Context(), []string{"kill", "web", "api"}); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if slices.Contains(r.seen(), "provider.StopProcess") {
		t.Errorf("kill drove %v, want no guest call", r.seen())
	}
	if p := d.repoSvc.(*fakeLifecycleRepo).sb.Processes[0]; !p.Killed {
		t.Errorf("the record holds %+v, want api marked killed", p)
	}
}

func TestKillRefusals(t *testing.T) {
	for args, want := range map[string]string{
		"kill web":            "kill takes one sandbox id or name and one process name, got ",
		"kill web api worker": "kill takes one sandbox id or name and one process name, got ",
		"kill web ghost":      "it has no process ghost; shard ps web lists the ones it has",
	} {
		var out bytes.Buffer

		app, _, _ := newKillApp(t, &out, stopped())

		err := app.Run(t.Context(), strings.Fields(args))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s returned %v, want %q", args, err, want)
		}
	}
}
