package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
)

func TestInspectPrintsTheRecordAsJSON(t *testing.T) {
	var out bytes.Buffer

	sb := stopped()
	sb.Processes = []models.Process{{Name: "api", Command: []string{"python", "app.py"}, Status: models.ProcessStatus{State: models.ProcessStopped, Exit: &models.ExitStatus{Code: 3}}}}
	app, _ := newClientApp(t, &out, sb)

	if err := app.Run(t.Context(), []string{"inspect", "web"}); err != nil {
		t.Fatalf("inspect: %v", err)
	}

	var got client.Inspection
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("inspect printed something that is not JSON: %v\n%s", err, out.String())
	}
	if got.ID != "sandbox1" || got.State != models.StateStopped || len(got.Processes) != 1 || got.Processes[0].Status.Exit == nil || got.Processes[0].Status.Exit.Code != 3 {
		t.Errorf("inspect printed %+v", got)
	}
	if !strings.Contains(out.String(), `"state": "stopped"`) {
		t.Errorf("the state is not a top-level field jq can read:\n%s", out.String())
	}
}

func TestInspectNamesAMissingSandbox(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, models.Sandbox{})
	d.repoSvc.(*fakeLifecycleRepo).missing = true

	err := app.Run(t.Context(), []string{"inspect", "ghost"})
	if err == nil || err.Error() != "no sandbox ghost" {
		t.Errorf("inspect returned %v, want 'no sandbox ghost'", err)
	}
}
