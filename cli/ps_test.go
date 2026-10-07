package cli

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

// newPsApp is a running sandbox whose record ran three processes, and whose guest reports on them.
func newPsApp(t *testing.T, out *bytes.Buffer) App {
	t.Helper()

	sb := running()
	sb.Processes = []models.Process{
		{Name: "api", Command: []string{"python", "app.py"}, Restart: models.RestartSpec{Policy: models.RestartOnFailure, Retries: 5, Backoff: 1}, Status: models.ProcessStatus{State: models.ProcessRunning}},
		{Name: "worker", Command: []string{"sleep", "60"}, Restart: models.RestartSpec{Policy: models.RestartAlways, Backoff: 1}, Status: models.ProcessStatus{State: models.ProcessRunning}},
		{Name: "job", Command: []string{"sh", "-c", "true"}, Restart: models.RestartSpec{Policy: models.RestartNo}, Status: models.ProcessStatus{State: models.ProcessRunning}},
	}
	app, d := newLifecycleApp(t, out, &recorder{}, sb)
	provider := d.providerSvc.(*fakeLifecycleProvider)
	provider.report("api", models.ProcessStatus{State: models.ProcessRestarting, Restarts: 2, Exit: &models.ExitStatus{Code: 1}})
	provider.report("job", models.ProcessStatus{State: models.ProcessKilled, Exit: &models.ExitStatus{Signal: 15}})
	// Guest root can write any name into the table, and ps shows only what the record ran.
	provider.report("forged", models.ProcessStatus{State: models.ProcessRunning})

	return app
}

func TestPsPrintsEachProcessAsTheGuestReportsIt(t *testing.T) {
	var out bytes.Buffer

	app := newPsApp(t, &out)

	if err := app.Run(t.Context(), []string{"ps", "sandbox1"}); err != nil {
		t.Fatalf("ps: %v", err)
	}

	var lines []string
	for line := range strings.Lines(out.String()) {
		lines = append(lines, strings.Join(strings.Fields(line), " "))
	}
	want := []string{
		"NAME STATE RESTARTS EXIT POLICY COMMAND",
		"api restarting 2/5 1 on-failure python app.py",
		"worker running 0 - always sleep 60",
		"job killed 0 signal 15 no sh -c true",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("ps printed\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestPsPrintsJSON(t *testing.T) {
	var out bytes.Buffer

	app := newPsApp(t, &out)

	if err := app.Run(t.Context(), []string{"ps", "--format", "json", "sandbox1"}); err != nil {
		t.Fatalf("ps --format json: %v", err)
	}

	var procs []models.Process
	if err := json.Unmarshal(out.Bytes(), &procs); err != nil {
		t.Fatalf("ps printed something that is not JSON: %v\n%s", err, out.String())
	}
	if len(procs) != 3 || procs[0].Name != "api" || procs[0].Status.Restarts != 2 {
		t.Errorf("ps printed %+v, want the three processes the record ran", procs)
	}
}

func TestPsTakesOneSandbox(t *testing.T) {
	for _, args := range [][]string{{"ps"}, {"ps", "web", "api"}} {
		var out bytes.Buffer

		err := newApp(t, &out).Run(t.Context(), args)
		if err == nil || !strings.HasPrefix(err.Error(), "ps takes one sandbox id or name, got ") {
			t.Errorf("%v returned %v, want the one sandbox it takes", args, err)
		}
	}
}
